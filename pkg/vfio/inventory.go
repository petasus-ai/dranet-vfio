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

package vfio

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/names"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	// AttrDomain is the attribute domain of this driver's device attributes.
	AttrDomain = "vfio.petasus.io"

	// AttrResourceName carries the pool's logical resource name; the
	// management platform joins DeviceClasses and networks on it.
	AttrResourceName = "k8s.cni.cncf.io/resourceName"

	// DefaultResourceNamePrefix prefixes pool names into resource names.
	DefaultResourceNamePrefix = "petasus.io/"

	defaultReloadInterval = 30 * time.Second
)

// Spec describes one published device for the prepare path.
type Spec struct {
	Pool         string
	PCIAddress   string
	PFPCIAddress string
	PFName       string
	VFIndex      int
	Kind         string
	LinkType     string
	Class        string
	IOMMUGroup   int
}

// Inventory publishes vfio-pci-bound network PCI functions as DRA devices.
// Hardware is discovered from sysfs; which devices are exposed — and under
// which pool/resource name — is defined in a config file (mounted from a
// ConfigMap) that is re-read periodically, so pool changes and newly bound
// devices propagate without a driver restart.
type Inventory struct {
	configPath     string
	nodeName       string
	reloadInterval time.Duration
	sysfs          SysfsOps

	mu        sync.RWMutex
	devices   map[string]resourceapi.Device
	specs     map[string]Spec
	published []resourceapi.Device

	notifications chan []resourceapi.Device
	rescan        chan struct{}
}

// Option customizes the Inventory.
type Option func(*Inventory)

// WithReloadInterval overrides how often the config file is re-read.
func WithReloadInterval(d time.Duration) Option {
	return func(inv *Inventory) {
		inv.reloadInterval = d
	}
}

// WithSysfs overrides the sysfs implementation (for testing).
func WithSysfs(s SysfsOps) Option {
	return func(inv *Inventory) {
		inv.sysfs = s
	}
}

// New creates an Inventory that discovers vfio-pci-bound network devices
// and advertises those matched by a pool defined in configPath.
func New(configPath, nodeName string, opts ...Option) *Inventory {
	inv := &Inventory{
		configPath:     configPath,
		nodeName:       nodeName,
		reloadInterval: defaultReloadInterval,
		sysfs:          NewSysfs(),
		devices:        map[string]resourceapi.Device{},
		specs:          map[string]Spec{},
		notifications:  make(chan []resourceapi.Device, 1),
		rescan:         make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(inv)
	}
	return inv
}

// Run keeps the published device list in sync with the config file and the
// host's vfio-pci bindings until the context is cancelled. The periodic
// reload doubles as a hardware rescan, so newly bound devices appear
// without a restart.
func (inv *Inventory) Run(ctx context.Context) error {
	inv.sync(true)
	ticker := time.NewTicker(inv.reloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			inv.sync(false)
		case <-inv.rescan:
			inv.sync(false)
		}
	}
}

// sync reloads the config, rebuilds the device list and notifies the
// publisher when the list changed (or unconditionally on the first pass, so
// an empty device set still results in an initial publish).
func (inv *Inventory) sync(force bool) {
	devices, specs := inv.buildDevices()

	inv.mu.Lock()
	changed := !reflect.DeepEqual(devices, inv.published)
	if changed || force {
		inv.published = devices
		inv.devices = map[string]resourceapi.Device{}
		for _, dev := range devices {
			inv.devices[dev.Name] = dev
		}
		inv.specs = specs
	}
	inv.mu.Unlock()

	if changed || force {
		// Coalesce: drop a stale pending notification before sending the new one.
		select {
		case <-inv.notifications:
		default:
		}
		inv.notifications <- devices
	}
}

func (inv *Inventory) buildDevices() ([]resourceapi.Device, map[string]Spec) {
	devices := []resourceapi.Device{}
	specs := map[string]Spec{}

	config, err := LoadConfig(inv.configPath)
	if err != nil {
		klog.Errorf("vfio inventory: failed to load config %s, advertising nothing: %v", inv.configPath, err)
		return devices, specs
	}

	bdfs, err := inv.sysfs.ListVFIODevices()
	if err != nil {
		klog.Errorf("vfio inventory: failed to list vfio-pci devices, advertising nothing: %v", err)
		return devices, specs
	}

	for _, bdf := range bdfs {
		spec, err := inv.describeDevice(bdf)
		if err != nil {
			klog.Warningf("vfio inventory: skipping device %s: %v", bdf, err)
			continue
		}
		if spec == nil {
			// Not a network-class device: vfio-pci also carries GPUs and
			// other passthrough functions owned by different drivers.
			continue
		}

		pool := inv.matchPool(config, spec)
		if pool == nil {
			klog.V(4).Infof("vfio inventory: device %s (%s %s, PF %s) matches no pool, not advertising",
				bdf, spec.LinkType, spec.Kind, spec.PFName)
			continue
		}
		spec.Pool = pool.Name

		resourceName := pool.ResourceName
		if resourceName == "" {
			resourceName = DefaultResourceNamePrefix + pool.Name
		}

		device := inv.buildDevice(spec, resourceName)
		devices = append(devices, device)
		specs[device.Name] = *spec
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	return devices, specs
}

// describeDevice reads the sysfs facts of one vfio-pci-bound function.
// A nil Spec with nil error means the device is not a network controller.
func (inv *Inventory) describeDevice(bdf string) (*Spec, error) {
	class, err := inv.sysfs.GetPciClass(bdf)
	if err != nil {
		return nil, err
	}
	if !isNetworkClass(class) {
		return nil, nil
	}
	isVf, err := inv.sysfs.IsVf(bdf)
	if err != nil {
		return nil, err
	}

	spec := &Spec{
		PCIAddress: bdf,
		Class:      class,
		Kind:       KindPF,
		VFIndex:    -1,
		LinkType:   linkTypeOf(class),
	}
	if isVf {
		spec.Kind = KindVF
		pfPci, err := inv.sysfs.GetPfPci(bdf)
		if err != nil {
			return nil, err
		}
		spec.PFPCIAddress = pfPci
		// The PF may itself be vfio-pci-bound and then has no netdev; PF
		// pools select such VFs by PCI address instead.
		if pfName, err := inv.sysfs.GetPfName(pfPci); err == nil {
			spec.PFName = pfName
		}
		vfIndex, err := inv.sysfs.GetVfIndex(pfPci, bdf)
		if err != nil {
			return nil, err
		}
		spec.VFIndex = vfIndex
	}

	group, err := inv.sysfs.GetIOMMUGroup(bdf)
	if err != nil {
		// Without an IOMMU group there is no /dev/vfio/<group> to hand to
		// the VM, so the device is unusable.
		return nil, err
	}
	spec.IOMMUGroup = group
	return spec, nil
}

// matchPool returns the first pool a device belongs to, warning when the
// configuration makes several pools claim the same device.
func (inv *Inventory) matchPool(config *Config, spec *Spec) *PoolConfig {
	var matched *PoolConfig
	for i := range config.Pools {
		pool := &config.Pools[i]
		if !pool.Matches(inv.nodeName, spec.Kind, spec.LinkType, spec.PFName, spec.PFPCIAddress, spec.PCIAddress) {
			continue
		}
		if matched == nil {
			matched = pool
			continue
		}
		klog.Warningf("vfio inventory: device %s matches pools %q and %q, keeping %q",
			spec.PCIAddress, matched.Name, pool.Name, matched.Name)
	}
	return matched
}

func (inv *Inventory) buildDevice(spec *Spec, resourceName string) resourceapi.Device {
	device := resourceapi.Device{
		Name: names.NormalizePCIAddress(spec.PCIAddress),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			AttrResourceName: {StringValue: ptr.To(resourceName)},
			// The standard PCI address attribute is what KubeVirt's DRA
			// hostdevice path reads (via the KEP-5304 metadata file) to
			// build the libvirt <hostdev>.
			deviceattribute.StandardDeviceAttributePCIBusID: {StringValue: ptr.To(spec.PCIAddress)},
			AttrDomain + "/pool":                            {StringValue: ptr.To(spec.Pool)},
			AttrDomain + "/kind":                            {StringValue: ptr.To(spec.Kind)},
			AttrDomain + "/linkType":                        {StringValue: ptr.To(spec.LinkType)},
			AttrDomain + "/pciClass":                        {StringValue: ptr.To(spec.Class)},
			AttrDomain + "/iommuGroup":                      {IntValue: ptr.To(int64(spec.IOMMUGroup))},
			AttrDomain + "/deviceType":                      {StringValue: ptr.To("vfio-pci")},
		},
	}
	if spec.PFName != "" {
		device.Attributes[AttrDomain+"/pfName"] = resourceapi.DeviceAttribute{StringValue: ptr.To(spec.PFName)}
	}
	if spec.PFPCIAddress != "" {
		device.Attributes[AttrDomain+"/pfPciAddress"] = resourceapi.DeviceAttribute{StringValue: ptr.To(spec.PFPCIAddress)}
	}
	if spec.Kind == KindVF {
		device.Attributes[AttrDomain+"/vfIndex"] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(spec.VFIndex))}
	}
	if numa, err := inv.sysfs.GetNumaNode(spec.PCIAddress); err == nil && numa >= 0 {
		device.Attributes[AttrDomain+"/numaNode"] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(numa))}
	}
	if vendor, err := inv.sysfs.GetVendorID(spec.PCIAddress); err == nil && vendor != "" {
		device.Attributes[AttrDomain+"/vendor"] = resourceapi.DeviceAttribute{StringValue: ptr.To(vendor)}
	}
	if deviceID, err := inv.sysfs.GetDeviceID(spec.PCIAddress); err == nil && deviceID != "" {
		device.Attributes[AttrDomain+"/deviceID"] = resourceapi.DeviceAttribute{StringValue: ptr.To(deviceID)}
	}
	// Best effort: topology alignment hint, resolved from the live /sys.
	if pcieRootAttr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(spec.PCIAddress); err == nil {
		device.Attributes[pcieRootAttr.Name] = pcieRootAttr.Value
	}
	return device
}

func linkTypeOf(class string) string {
	switch {
	case isEthernetClass(class):
		return LinkTypeEthernet
	case isInfiniBandClass(class):
		return LinkTypeInfiniband
	default:
		return LinkTypeOther
	}
}

// GetResources returns the channel the publisher consumes device lists from.
func (inv *Inventory) GetResources(_ context.Context) <-chan []resourceapi.Device {
	return inv.notifications
}

// GetDevice returns the published device by name.
func (inv *Inventory) GetDevice(deviceName string) (resourceapi.Device, bool) {
	inv.mu.RLock()
	defer inv.mu.RUnlock()
	dev, ok := inv.devices[deviceName]
	return dev, ok
}

// GetVfioSpec returns the sysfs facts of a published device.
func (inv *Inventory) GetVfioSpec(deviceName string) (Spec, bool) {
	inv.mu.RLock()
	defer inv.mu.RUnlock()
	spec, ok := inv.specs[deviceName]
	return spec, ok
}

// GetNetInterfaceName never resolves: a vfio-pci-bound device has no
// kernel netdev.
func (inv *Inventory) GetNetInterfaceName(deviceName string) (string, error) {
	return "", fmt.Errorf("device %s is vfio-pci-bound and has no netdev", deviceName)
}

// IsIBOnlyDevice is always false: even InfiniBand devices are handled by
// the vfio path, not the RDMA one.
func (inv *Inventory) IsIBOnlyDevice(_ string) bool {
	return false
}

// GetRDMADeviceName never resolves: a vfio-pci-bound device exposes no
// host RDMA device.
func (inv *Inventory) GetRDMADeviceName(deviceName string) (string, error) {
	return "", fmt.Errorf("device %s has no RDMA device", deviceName)
}

// GetDeviceConfig returns no provider-side configuration.
func (inv *Inventory) GetDeviceConfig(_ string) (*apis.NetworkConfig, bool) {
	return nil, false
}

// RequestRescan triggers an immediate config reload and republish check.
func (inv *Inventory) RequestRescan() {
	select {
	case inv.rescan <- struct{}{}:
	default:
	}
}

// GetProfileConfig is not supported; profiles must not be requested.
func (inv *Inventory) GetProfileConfig(deviceName string, _ types.UID, _ *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	return nil, fmt.Errorf("profiles are not supported (device %s)", deviceName)
}

// ReleaseProfileConfig is a no-op as profiles are not supported.
func (inv *Inventory) ReleaseProfileConfig(_ string, _ types.UID, _ *apis.NetworkConfig) error {
	return nil
}

// String renders the inventory state for debug logging.
func (inv *Inventory) String() string {
	inv.mu.RLock()
	defer inv.mu.RUnlock()
	names := make([]string, 0, len(inv.devices))
	for name := range inv.devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
