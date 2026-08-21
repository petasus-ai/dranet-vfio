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

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/pkg/vfio"
	"sigs.k8s.io/dranet/pkg/vfio/metadata"
)

const (
	// qemuUID/qemuGID own the vfio character devices inside the container
	// so the qemu process in KubeVirt's virt-launcher (uid/gid 107) can
	// open them.
	qemuUID = 107
	qemuGID = 107

	// vfioDevFileMode is the mode of the vfio character device nodes
	// created in the container.
	vfioDevFileMode = 0o660

	// vfioContainerPath is the VFIO container device every consumer needs
	// in addition to its group device.
	vfioContainerPath = "/dev/vfio/vfio"
)

// vfioInventory is the extra contract a vfio inventory provides on top of
// the generic inventoryDB interface.
type vfioInventory interface {
	GetVfioSpec(deviceName string) (vfio.Spec, bool)
}

// vfioSpecOf resolves the sysfs facts of a device, when the inventory
// serves vfio-pci devices and the device belongs to it.
func vfioSpecOf(db inventoryDB, deviceName string) (vfio.Spec, bool) {
	vdb, ok := db.(vfioInventory)
	if !ok {
		return vfio.Spec{}, false
	}
	return vdb.GetVfioSpec(deviceName)
}

// prepareVfioDevice handles one allocated vfio-pci device: validate it,
// apply the claim's VF settings on the host, and record everything the NRI
// hooks and virt-launcher need — the /dev/vfio character devices and the
// KEP-5304 metadata file carrying the PCI address.
func (np *NetworkDriver) prepareVfioDevice(claim *resourceapi.ResourceClaim, podUID types.UID, requestName string, result resourceapi.DeviceRequestAllocationResult, spec vfio.Spec) error {
	params, err := vfio.ParseClaimParams(claim, np.driverName, requestName)
	if err != nil {
		return err
	}

	if err := np.vfioConfigurer.ValidateIOMMUGroup(spec.IOMMUGroup, spec.PCIAddress); err != nil {
		return err
	}

	// Reuse previously captured original VF state: a kubelet retry — or a
	// stale entry left by a dead pod the device was reallocated from —
	// already applied settings, so reading the kernel now would record our
	// own values as the host's originals.
	prior := np.findPriorVfioState(podUID, result.Device)

	state, err := np.vfioConfigurer.Apply(spec, params, prior)
	if err != nil {
		return fmt.Errorf("failed to configure VFIO device %s: %w", result.Device, err)
	}
	undo := func() {
		for _, restoreErr := range np.vfioConfigurer.Restore(state) {
			klog.Errorf("failed to undo VFIO device %s configuration: %v", result.Device, restoreErr)
		}
	}

	devices, err := vfioCharDevices(spec.IOMMUGroup)
	if err != nil {
		undo()
		return err
	}

	deviceCfg := DeviceConfig{
		Claim: types.NamespacedName{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
		VfioPCIAddress: spec.PCIAddress,
		VfioState:      state,
		VfioDevices:    devices,
	}
	if device, ok := np.netdb.GetDevice(result.Device); ok {
		deviceCfg.DeviceSnapshot = &device
	} else {
		klog.Warningf("Failed to find device %s in inventory for claim %s", result.Device, claim.UID)
	}

	// Write the KEP-5304 metadata file virt-launcher resolves the PCI
	// address from; the NRI hook bind-mounts it into the containers.
	ref := metadata.NewClaimRef(claim)
	var attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	if deviceCfg.DeviceSnapshot != nil {
		attributes = deviceCfg.DeviceSnapshot.Attributes
	}
	hostPath, err := np.vfioMetadata.Write(ref, requestName, []metadata.Device{{
		Driver:     np.driverName,
		Pool:       np.nodeName,
		Name:       result.Device,
		Attributes: attributes,
	}})
	if err != nil {
		undo()
		return fmt.Errorf("failed to write device metadata for %s: %w", result.Device, err)
	}
	deviceCfg.MetadataMount = &BindMount{
		HostPath:      hostPath,
		ContainerPath: np.vfioMetadata.ContainerPath(ref, requestName),
	}

	if err := np.podConfigStore.SetDeviceConfig(podUID, result.Device, deviceCfg); err != nil {
		undo()
		return fmt.Errorf("failed to persist device config for pod %s device %s: %v", podUID, result.Device, err)
	}
	klog.V(4).Infof("VFIO claim resources for pod %s : %#v", podUID, deviceCfg)
	return nil
}

// findPriorVfioState returns the host state an earlier prepare recorded for
// the same device, preferring the current pod's own entry (kubelet retry)
// over an entry left under another pod (dead sandbox the device was
// reallocated from).
func (np *NetworkDriver) findPriorVfioState(podUID types.UID, deviceName string) *vfio.HostState {
	if cfg, ok := np.podConfigStore.GetDeviceConfig(podUID, deviceName); ok && cfg.VfioState != nil {
		return cfg.VfioState
	}
	for _, uid := range np.podConfigStore.ListPods() {
		if uid == podUID {
			continue
		}
		if cfg, ok := np.podConfigStore.GetDeviceConfig(uid, deviceName); ok && cfg.VfioState != nil {
			klog.Warningf("device %s was reallocated from pod %s: reusing its recorded original VF state", deviceName, uid)
			return cfg.VfioState
		}
	}
	return nil
}

// restoreVfioDevice puts the host-side VF configuration back to what it was
// before prepare, from persisted state only — it works even after the
// device's pool was removed from the driver configuration.
func (np *NetworkDriver) restoreVfioDevice(deviceName string, devCfg DeviceConfig) {
	for _, err := range np.vfioConfigurer.Restore(devCfg.VfioState) {
		klog.Errorf("failed to restore VFIO device %s: %v", deviceName, err)
	}
}

// vfioCharDevices shapes /dev/vfio/vfio and the group's /dev/vfio/<N> for
// container injection, owned by qemu.
func vfioCharDevices(iommuGroup int) ([]LinuxDevice, error) {
	var devices []LinuxDevice
	for _, path := range []string{vfioContainerPath, fmt.Sprintf("/dev/vfio/%d", iommuGroup)} {
		dev, err := GetDeviceInfo(path)
		if err != nil {
			return nil, fmt.Errorf("failed to stat vfio device %s: %w", path, err)
		}
		dev.FileMode = vfioDevFileMode
		dev.UID = qemuUID
		dev.GID = qemuGID
		devices = append(devices, dev)
	}
	return devices, nil
}
