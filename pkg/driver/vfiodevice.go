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
	"os"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/pkg/vfio"
	"sigs.k8s.io/dranet/pkg/vfio/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
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
// record and apply the claim's VF settings on the host, and return the CDI
// device that hands the /dev/vfio character devices to the pod, with the
// device metadata (PCI address) the kubeletplugin helper writes for
// virt-launcher. The CDI device is claim-scoped, so kubelet also injects it
// into a pod that reuses the prepared claim without a new prepare (a
// replacement launcher on the same node while the old sandbox still exists).
func (np *NetworkDriver) prepareVfioDevice(claim *resourceapi.ResourceClaim, podUID types.UID, result resourceapi.DeviceRequestAllocationResult, spec vfio.Spec) (cdispec.Device, kubeletplugin.Device, error) {
	params, err := vfio.ParseClaimParams(claim, np.driverName, result)
	if err != nil {
		return cdispec.Device{}, kubeletplugin.Device{}, err
	}

	if err := np.vfioConfigurer.ValidateIOMMUGroup(spec.IOMMUGroup, spec.PCIAddress); err != nil {
		return cdispec.Device{}, kubeletplugin.Device{}, err
	}

	// Reuse previously captured original VF state: a kubelet retry — or a
	// stale entry left by a dead pod the device was reallocated from —
	// already applied settings, so reading the kernel now would record our
	// own values as the host's originals.
	prior := np.findPriorVfioState(podUID, result.Device)

	state, err := np.vfioConfigurer.Plan(spec, params, prior)
	if err != nil {
		return cdispec.Device{}, kubeletplugin.Device{}, fmt.Errorf("failed to configure VFIO device %s: %w", result.Device, err)
	}

	devices, err := vfioCharDevices(spec.IOMMUGroup)
	if err != nil {
		return cdispec.Device{}, kubeletplugin.Device{}, err
	}

	deviceCfg := DeviceConfig{
		Claim: types.NamespacedName{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
		CarryDeviceConfig: CarryDeviceConfig{
			VfioPCIAddress: spec.PCIAddress,
			VfioState:      state,
			VfioDevices:    devices,
		},
	}
	// Workloads see the published attributes; the PCI address comes from the
	// spec this prepare used, whatever a rescan did to the inventory since.
	attributes := map[string]resourceapi.DeviceAttribute{}
	if device, ok := np.netdb.GetDevice(result.Device); ok {
		deviceCfg.DeviceSnapshot = &device
		for name, attr := range device.Attributes {
			attributes[string(name)] = attr
		}
	} else {
		klog.Warningf("Failed to find device %s in inventory for claim %s", result.Device, claim.UID)
	}
	attributes[string(deviceattribute.StandardDeviceAttributePCIBusID)] = resourceapi.DeviceAttribute{StringValue: ptr.To(spec.PCIAddress)}

	cdiDevice := vfioCDIDevice(cdi.DeviceName(string(claim.UID), resourceclaim.BaseRequestRef(result.Request), result.Device), devices)
	deviceCfg.CDIDeviceID = np.vfioCDI.DeviceID(cdiDevice.Name)

	// Stored before the host changes, so a driver that dies inside Apply
	// finds the originals again on the retry and unprepare can restore them.
	if err := np.podConfigStore.SetDeviceConfig(podUID, result.Device, deviceCfg); err != nil {
		return cdispec.Device{}, kubeletplugin.Device{}, fmt.Errorf("failed to persist device config for pod %s device %s: %v", podUID, result.Device, err)
	}
	// The entry keeps the originals on failure: an earlier attempt may have
	// changed the host, and kubelet unprepares a claim whose prepare failed.
	if err := np.vfioConfigurer.Apply(state, params); err != nil {
		return cdispec.Device{}, kubeletplugin.Device{}, fmt.Errorf("failed to configure VFIO device %s: %w", result.Device, err)
	}
	klog.V(4).Infof("VFIO claim resources for pod %s : %#v", podUID, deviceCfg)
	return cdiDevice, kubeletplugin.Device{
		Requests:     []string{result.Request},
		PoolName:     result.Pool,
		DeviceName:   result.Device,
		CDIDeviceIDs: []string{deviceCfg.CDIDeviceID},
		Metadata:     &kubeletplugin.DeviceMetadata{Attributes: attributes},
	}, nil
}

// vfioCDIDevice is the CDI device carrying one vfio device's character
// devices, owned by qemu.
func vfioCDIDevice(name string, devices []LinuxDevice) cdispec.Device {
	dev := cdispec.Device{Name: name}
	for _, d := range devices {
		mode := os.FileMode(d.FileMode)
		uid, gid := d.UID, d.GID
		dev.ContainerEdits.DeviceNodes = append(dev.ContainerEdits.DeviceNodes, &cdispec.DeviceNode{
			Path:        d.Path,
			HostPath:    d.Path,
			Type:        d.Type,
			Major:       d.Major,
			Minor:       d.Minor,
			FileMode:    &mode,
			Permissions: "rw",
			UID:         &uid,
			GID:         &gid,
		})
	}
	return dev
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
