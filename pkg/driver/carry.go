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
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	metadatav1alpha1 "k8s.io/dynamic-resource-allocation/api/metadata/v1alpha1"
	metadatav1beta1 "k8s.io/dynamic-resource-allocation/api/metadata/v1beta1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1"
	drapbv1beta1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/utils/set"
	"sigs.k8s.io/dranet/pkg/vfio"
	"sigs.k8s.io/dranet/pkg/vfio/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// carryState is embedded in NetworkDriver.
type carryState struct {
	// VFIO passthrough machinery: host-side VF configuration and the
	// claim-scoped CDI specs that hand the /dev/vfio nodes to the pod.
	vfLockDir      string
	vfioConfigurer *vfio.Configurer
	cdiDir         string
	vfioCDI        *cdi.Writer

	// vfioRestoreRetried holds the claims whose unprepare already failed
	// once on a restore.
	vfioRestoreRetried set.Set[types.UID]
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

	// CDIDeviceID is the claim-scoped CDI device returned from prepare.
	CDIDeviceID string `json:"cdiDeviceID,omitempty"`
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
func (np *NetworkDriver) initCarry() {
	if np.vfLockDir == "" {
		np.vfLockDir = "/var/run/dranet-vfio"
	}
	np.vfioConfigurer = vfio.NewConfigurer(vfio.NewSysfs(), vfio.NewNetlink(), np.vfLockDir)
	np.vfioCDI = &cdi.Writer{DriverName: np.driverName, SpecDir: np.cdiDir}
	np.vfioRestoreRetried = set.New[types.UID]()
}

// vfioMetadataVersions are the KEP-5304 metadata versions the helper writes,
// newest first: a reader takes the first one it understands, and KubeVirt
// v1.9.1 reads v1alpha1.
var vfioMetadataVersions = []schema.GroupVersion{metadatav1beta1.SchemeGroupVersion, metadatav1alpha1.SchemeGroupVersion}

// carryKubeletOptions is appended to the kubelet plugin options, so it
// overrides the upstream ones.
func (np *NetworkDriver) carryKubeletOptions() []kubeletplugin.Option {
	return []kubeletplugin.Option{
		// Kubelet 1.36 re-dials a health stream that is absent or declined
		// every few seconds and logs each attempt; WatchHealthStatus holds it
		// open.
		kubeletplugin.HealthService(true),
		// The helper writes the metadata file of each prepared request below
		// the plugin data directory and a CDI spec bind-mounting it, where
		// virt-launcher reads the PCI address from. The host path is the
		// mount source: the DaemonSet mounts /var/lib/kubelet/plugins 1:1.
		kubeletplugin.EnableDeviceMetadata(true, vfioMetadataVersions),
		kubeletplugin.CDIDirectory(np.cdiDir),
		kubeletplugin.GRPCInterceptor(requireDeviceMetadata(np.driverName)),
	}
}

// requireDeviceMetadata fails every prepared claim of a NodePrepareResources
// response whose devices carry none of the helper's metadata CDI devices
// (<driver>/metadata=<claim UID>_<request>). The helper adds them once it has
// written the claim's metadata and only logs a failed write, so kubelet would
// keep the claim prepared while virt-launcher finds no PCI address. Kubelet
// retries a failed prepare.
func requireDeviceMetadata(driverName string) grpc.UnaryServerInterceptor {
	kind := driverName + "/metadata="
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			return resp, err
		}
		switch r := resp.(type) {
		case *drapbv1.NodePrepareResourcesResponse:
			for uid, claim := range r.GetClaims() {
				if claim.GetError() == "" && lacksMetadata(claim.GetDevices(), kind) {
					claim.Error = metadataNotWritten(uid)
				}
			}
		case *drapbv1beta1.NodePrepareResourcesResponse:
			for uid, claim := range r.GetClaims() {
				if claim.GetError() == "" && lacksMetadata(claim.GetDevices(), kind) {
					claim.Error = metadataNotWritten(uid)
				}
			}
		}
		return resp, nil
	}
}

// lacksMetadata reports whether prepared devices carry no CDI device of the
// metadata kind.
func lacksMetadata[D interface{ GetCdiDeviceIds() []string }](devices []D, kind string) bool {
	if len(devices) == 0 {
		return false
	}
	for _, device := range devices {
		for _, id := range device.GetCdiDeviceIds() {
			if strings.HasPrefix(id, kind) {
				return false
			}
		}
	}
	return true
}

func metadataNotWritten(claimUID string) string {
	return fmt.Sprintf("claim %s: the device metadata was not written (see the driver log)", claimUID)
}

// carrySlices lays the published devices out as the node pool's
// ResourceSlices, with the IOMMU-group counter sets they consume.
func carrySlices(devices []resourceapi.Device) []resourceslice.Slice {
	return vfio.Slices(devices)
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

// carryClaim collects what the carried devices of one claim prepared.
type carryClaim struct {
	cdiDevices []cdispec.Device
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
	cdiDevice, prepared, err := np.prepareVfioDevice(claim, podUID, result, spec)
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

// unprepareCarryClaim runs once per unprepared claim, before its entries
// leave the store: it restores the host state its vfio devices changed and
// removes its CDI spec. A failed restore fails the claim's first unprepare,
// so kubelet retries it; a second failure is logged and the claim let go, so
// a broken VF never blocks the pod's teardown.
func (np *NetworkDriver) unprepareCarryClaim(claim kubeletplugin.NamespacedObject) error {
	var errs []error
	for _, podUID := range np.podConfigStore.ListPods() {
		podCfg, _ := np.podConfigStore.GetPodConfig(podUID)
		for deviceName, cfg := range podCfg.DeviceConfigs {
			if cfg.Claim != claim.NamespacedName || cfg.VfioState == nil {
				continue
			}
			if np.vfioMovedOn(claim.NamespacedName, cfg.VfioPCIAddress) {
				klog.Infof("device %s of claim %v was reallocated; its new claim restores it", deviceName, claim.NamespacedName)
				continue
			}
			for _, err := range np.vfioConfigurer.Restore(cfg.VfioState) {
				errs = append(errs, fmt.Errorf("device %s: %w", deviceName, err))
			}
		}
	}
	if len(errs) > 0 {
		err := fmt.Errorf("failed to restore the host state of claim %v: %w", claim.NamespacedName, errors.Join(errs...))
		if !np.vfioRestoreRetried.Has(claim.UID) {
			np.vfioRestoreRetried.Insert(claim.UID)
			return err
		}
		klog.Errorf("giving up: %v", err)
	}
	np.vfioRestoreRetried.Delete(claim.UID)

	if np.vfioCDI != nil {
		if err := np.vfioCDI.Remove(string(claim.UID)); err != nil {
			klog.Errorf("failed to remove CDI spec for claim %v: %v", claim.NamespacedName, err)
		}
	}
	return nil
}

// vfioMovedOn reports whether another claim's entry holds host state for
// the device at pciAddress: the device was reallocated before this claim
// was unprepared, and restoring now would reset the new owner's VF.
func (np *NetworkDriver) vfioMovedOn(claim types.NamespacedName, pciAddress string) bool {
	for _, podUID := range np.podConfigStore.ListPods() {
		podCfg, _ := np.podConfigStore.GetPodConfig(podUID)
		for _, cfg := range podCfg.DeviceConfigs {
			if cfg.Claim != claim && cfg.VfioState != nil && cfg.VfioPCIAddress == pciAddress {
				return true
			}
		}
	}
	return false
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
