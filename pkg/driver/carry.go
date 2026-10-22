/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

// This file holds what the vfio.petasus.io driver adds to the upstream
// driver. The upstream files reach it only through one-line hooks (the
// carry* names), so rebasing onto a newer upstream carries those calls and
// nothing else.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/utils/set"
	"sigs.k8s.io/dranet/pkg/vfio"
	"sigs.k8s.io/dranet/pkg/vfio/cdi"
	"sigs.k8s.io/dranet/pkg/vfio/metadata"
)

// carryState is embedded in NetworkDriver.
type carryState struct {
	// VFIO passthrough machinery: host-side VF configuration, the KEP-5304
	// metadata files virt-launcher reads PCI addresses from, and the
	// claim-scoped CDI specs that hand both to the pod.
	vfLockDir      string
	vfioConfigurer *vfio.Configurer
	vfioMetadata   *metadata.Writer
	cdiDir         string
	vfioCDI        *cdi.Writer

	// claimOpMu serializes prepare/unprepare claim processing. Kubelet can
	// overlap an in-flight unprepare with the next pod's prepare of the
	// same claim (a VM restart): without the lock, unprepare's store
	// cleanup races the fresh prepare and deletes the new pod's persisted
	// host state, so the final teardown never restores the device.
	claimOpMu sync.Mutex
}

// CarryDeviceConfig is embedded in DeviceConfig, so its fields sit at the top
// level of the checkpointed JSON.
type CarryDeviceConfig struct {
	// VfioPCIAddress is the PCI address of the vfio-pci-bound device
	// prepared for this claim; empty for non-vfio devices.
	VfioPCIAddress string `json:"vfioPciAddress,omitempty"`

	// VfioState records the host-side VF configuration prepare applied,
	// with the original settings unprepare restores.
	VfioState *vfio.HostState `json:"vfioState,omitempty"`

	// VfioDevices are the /dev/vfio character devices the CDI device hands
	// to the Pod's containers so qemu can open the passthrough device.
	VfioDevices []LinuxDevice `json:"vfioDevices,omitempty"`

	// MetadataMount bind-mounts the KEP-5304 device metadata file into the
	// Pod's containers; virt-launcher reads the PCI address from it.
	MetadataMount *BindMount `json:"metadataMount,omitempty"`

	// CDIDeviceID is the claim-scoped CDI device returned from prepare.
	CDIDeviceID string `json:"cdiDeviceID,omitempty"`
}

// BindMount describes one read-only bind mount from the host into the
// Pod's containers.
type BindMount struct {
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
}

// WithCDIDir sets the directory the container runtime reads generated CDI
// specs from.
func WithCDIDir(dir string) Option {
	return func(o *NetworkDriver) {
		o.cdiDir = dir
	}
}

// WithVFLockDir sets the directory holding the per-bridge flock files that
// serialize bridge-VLAN edits on switchdev VFs.
func WithVFLockDir(dir string) Option {
	return func(o *NetworkDriver) {
		o.vfLockDir = dir
	}
}

// initCarry runs in Start once the options are applied.
func (np *NetworkDriver) initCarry(driverPluginPath string) {
	if np.vfLockDir == "" {
		np.vfLockDir = "/var/run"
	}
	np.vfioConfigurer = vfio.NewConfigurer(vfio.NewSysfs(), vfio.NewNetlink(), np.vfLockDir)
	// The metadata files live below the plugin data directory: the host
	// path (the CDI bind-mount source) and the in-container path are
	// identical because the DaemonSet mounts /var/lib/kubelet/plugins 1:1.
	np.vfioMetadata = &metadata.Writer{DriverName: np.driverName, HostRoot: driverPluginPath}
	np.vfioCDI = &cdi.Writer{DriverName: np.driverName, SpecDir: np.cdiDir}
}

// carryKubeletOptions is appended to the kubelet plugin options, so it
// overrides the upstream ones.
func (np *NetworkDriver) carryKubeletOptions() []kubeletplugin.Option {
	// Kubelet 1.36 re-dials a health stream that is absent or declined every
	// few seconds and logs each attempt; WatchHealthStatus holds it open.
	return []kubeletplugin.Option{kubeletplugin.HealthService(true)}
}

// carryHealthReportInterval re-sends the empty health report well inside the
// 30s after which the kubelet plugin helper flags a silent stream as stale.
var carryHealthReportInterval = 10 * time.Second

// watchCarryHealth serves WatchHealthStatus. The driver tracks no per-device
// health, so the stream carries an empty report, re-sent periodically, and
// stays open until the kubelet closes it: kubelet 1.36 re-dials an absent or
// declined stream every few seconds and logs each attempt.
func (np *NetworkDriver) watchCarryHealth(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	ticker := time.NewTicker(carryHealthReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case reports <- kubeletplugin.DeviceHealthReport{}:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// lockCarryClaim serializes claim prepare and unprepare (claimOpMu); call
// it as defer np.lockCarryClaim()().
func (np *NetworkDriver) lockCarryClaim() func() {
	np.claimOpMu.Lock()
	return np.claimOpMu.Unlock
}

// carryClaim collects what the carried devices of one claim prepared.
type carryClaim struct {
	cdiDevices []cdi.Device
	devices    []kubeletplugin.Device
}

// prepareCarryDevice prepares a device the upstream netdev path cannot: a
// vfio-pci function has no netdev, and its opaque config uses this driver's
// flat VF-settings schema rather than apis.NetworkConfig. handled is false
// for any other device.
func (np *NetworkDriver) prepareCarryDevice(_ context.Context, claim *resourceapi.ResourceClaim, podUID types.UID, result resourceapi.DeviceRequestAllocationResult, carried *carryClaim) (bool, error) {
	spec, isVfio := vfioSpecOf(np.netdb, result.Device)
	if !isVfio {
		// After a restart the inventory has not scanned yet: a device this pod
		// already prepared must not pass through the upstream path, whose
		// stored-config shortcut would answer without the CDI device.
		if cfg, ok := np.podConfigStore.GetDeviceConfig(podUID, result.Device); ok && cfg.VfioPCIAddress != "" {
			return true, fmt.Errorf("vfio device %s is not in the inventory yet", result.Device)
		}
		return false, nil
	}
	cdiDevice, prepared, err := np.prepareVfioDevice(claim, podUID, result.Request, result, spec)
	if err != nil {
		return true, err
	}
	carried.cdiDevices = append(carried.cdiDevices, cdiDevice)
	carried.devices = append(carried.devices, prepared)
	return true, nil
}

// finishCarryClaim runs after every device of a claim prepared without
// error.
func (np *NetworkDriver) finishCarryClaim(claim *resourceapi.ResourceClaim, carried *carryClaim) error {
	if len(carried.cdiDevices) == 0 {
		return nil
	}
	if err := np.vfioCDI.Write(string(claim.UID), carried.cdiDevices); err != nil {
		return fmt.Errorf("failed to write CDI spec for claim %s: %w", claim.UID, err)
	}
	return nil
}

// deviceStoreKey is the PodConfigStore key of a device prepared by the
// upstream netdev path.
func (np *NetworkDriver) deviceStoreKey(_ *resourceapi.ResourceClaim, result resourceapi.DeviceRequestAllocationResult) string {
	return result.Device
}

// prepareCarryNetdev resolves the host interface of a device prepared by the
// upstream netdev path.
func (np *NetworkDriver) prepareCarryNetdev(_ *resourceapi.ResourceClaim, result resourceapi.DeviceRequestAllocationResult, _ *DeviceConfig) (string, error) {
	return np.netdb.GetNetInterfaceName(result.Device)
}

// rollbackCarryNetdev undoes prepareCarryNetdev when the device was not
// committed.
func (np *NetworkDriver) rollbackCarryNetdev(_ bool, _ DeviceConfig) {}

// unprepareCarryDevice runs for every stored device of an unprepared claim.
func (np *NetworkDriver) unprepareCarryDevice(_ kubeletplugin.NamespacedObject, deviceName string, devCfg DeviceConfig) {
	// Restore the host-side VF configuration a vfio prepare applied
	// (original MAC/VLAN/rates, representor detach).
	if devCfg.VfioState != nil {
		np.restoreVfioDevice(deviceName, devCfg)
	}
}

// unprepareCarryClaim runs once per unprepared claim; it is a no-op for a
// claim that carried no vfio devices.
func (np *NetworkDriver) unprepareCarryClaim(claim kubeletplugin.NamespacedObject) {
	if np.vfioMetadata != nil {
		if err := np.vfioMetadata.RemoveClaim(claim.Namespace, claim.Name); err != nil {
			klog.Errorf("failed to remove device metadata for claim %v: %v", claim.NamespacedName, err)
		}
	}
	if np.vfioCDI != nil {
		if err := np.vfioCDI.Remove(string(claim.UID)); err != nil {
			klog.Errorf("failed to remove CDI spec for claim %v: %v", claim.NamespacedName, err)
		}
	}
}

// addCarryContainerDevices adds the devices NRI hands to every container of
// the pod; vfio devices reach the containers through CDI instead.
func (np *NetworkDriver) addCarryContainerDevices(_ *api.ContainerAdjustment, _ DeviceConfig, _ set.Set[string]) {
}

// carryDeviceName is the published device name of a stored entry.
func carryDeviceName(storeKey string, _ DeviceConfig) string {
	return storeKey
}

// carryDeviceStatus completes the claim status of a device in RunPodSandbox.
func (np *NetworkDriver) carryDeviceStatus(config DeviceConfig, status *resourceapply.AllocatedDeviceStatusApplyConfiguration) {
	// A vfio passthrough device has no netdev: the CDI device is the
	// attachment, so the device is ready here.
	if config.VfioPCIAddress != "" {
		status.WithConditions(
			metav1apply.Condition().
				WithType("Ready").
				WithReason("VFIODeviceReady").
				WithStatus(metav1.ConditionTrue).
				WithLastTransitionTime(metav1.Now()),
		)
	}
}

// stopCarryDevice tears a carried device down in StopPodSandbox; handled is
// false for a device the upstream path detaches.
func (np *NetworkDriver) stopCarryDevice(_ context.Context, _ string, _ string, _ DeviceConfig) bool {
	return false
}
