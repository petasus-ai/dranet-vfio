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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// phys_port_name formats are produced by the kernel's devlink core in
// __devlink_port_phys_port_name_get() (net/devlink/port.c), one per
// DEVLINK_PORT_FLAVOUR_*. Every flavour that maps to a PCI function is
// prefixed with "c<N>" when the function belongs to an external
// controller (e.g. the host side of a BlueField DPU); ports of the local
// controller carry no prefix.
//
//	PHYSICAL (the uplink, i.e. the PF's own netdev): [l<N>]p<N>[s<N>]
//	PCI_PF:                                         [c<N>]pf<N>
//	PCI_VF:                                         [c<N>]pf<N>vf<N>
//	PCI_SF:                                         [c<N>]pf<N>sf<N>
//
// mlx5 additionally names the host-PF representor on a DPU "pf<N>hpf".
var (
	// uplinkPortNameRegex matches DEVLINK_PORT_FLAVOUR_PHYSICAL, the
	// flavour carried by a PF's own netdev in switchdev mode. Group 1 is
	// the external-controller number (empty for the local controller) and
	// group 2 is the port number.
	uplinkPortNameRegex = regexp.MustCompile(`^(?:c(\d+))?(?:l\d+)?p(\d+)(?:s\d+)?$`)

	// vfRepPortNameRegex matches DEVLINK_PORT_FLAVOUR_PCI_VF. Group 1 is
	// the external-controller number, group 2 the PF number and group 3
	// the VF number.
	vfRepPortNameRegex = regexp.MustCompile(`^(?:c(\d+))?pf(\d+)vf(\d+)$`)

	// pciRepPortNameRegex matches every PCI-function representor flavour
	// (PF, VF and SF, plus mlx5's "hpf"). It exists so PF lookup can rule
	// out representors instead of assuming everything that is not a VF
	// representor must be the PF.
	pciRepPortNameRegex = regexp.MustCompile(`^(?:c\d+)?pf\d+(?:vf\d+|sf\d+|hpf)?$`)

	// pfPortNameRegex matches a bare DEVLINK_PORT_FLAVOUR_PCI_PF name,
	// which some drivers use for the uplink instead of PHYSICAL.
	pfPortNameRegex = regexp.MustCompile(`^(?:c(\d+))?pf(\d+)$`)

	// bdfRegex matches a PCI address in extended BDF notation.
	bdfRegex = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
)

// PortID identifies an eswitch port by the PCI controller that owns it and
// the PF number within that controller. Controller is the "c<N>" prefix of
// phys_port_name and is empty for the local controller, which is the only
// one present on an ordinary host.
type PortID struct {
	Controller string
	PF         int
}

func (p PortID) String() string {
	if p.Controller == "" {
		return fmt.Sprintf("pf%d", p.PF)
	}
	return fmt.Sprintf("c%spf%d", p.Controller, p.PF)
}

// SysfsOps abstracts all sysfs read operations for testability.
type SysfsOps interface {
	ListVFIODevices() ([]string, error)
	IsVf(devPci string) (bool, error)
	GetPciClass(devPci string) (string, error)
	GetPfPci(vfPci string) (string, error)
	GetPfName(pfPci string) (string, error)
	GetVfIndex(pfPci, vfPci string) (int, error)
	GetDriver(devPci string) (string, error)
	GetVendorID(devPci string) (string, error)
	GetDeviceID(devPci string) (string, error)
	GetIOMMUGroup(devPci string) (int, error)
	ListIOMMUGroupDevices(group int) ([]string, error)
	GetNumaNode(devPci string) (int, error)
	GetRepresentor(pfPci string, port PortID, vfIndex int) (string, error)
	GetUplinkPort(pfName string) (PortID, error)
	GetBridgeVlanFiltering(bridge string) (bool, error)
	GetBridgeDefaultPvid(bridge string) (int, error)
}

// NewSysfs returns the real sysfs implementation using /sys.
func NewSysfs() SysfsOps { return &realSysfs{basePath: "/sys"} }

// NewSysfsWithBase returns a sysfs implementation with a custom base path (for testing).
func NewSysfsWithBase(basePath string) SysfsOps { return &realSysfs{basePath: basePath} }

type realSysfs struct {
	basePath string
}

// ListVFIODevices lists the PCI addresses of every device currently bound
// to the vfio-pci driver.
func (o *realSysfs) ListVFIODevices() ([]string, error) {
	driverDir := filepath.Join(o.basePath, "bus/pci/drivers/vfio-pci")
	entries, err := os.ReadDir(driverDir)
	if err != nil {
		if os.IsNotExist(err) {
			// vfio-pci module not loaded: no devices, not an error.
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read vfio-pci driver dir: %w", err)
	}
	var devices []string
	for _, e := range entries {
		if bdfRegex.MatchString(e.Name()) {
			devices = append(devices, e.Name())
		}
	}
	return devices, nil
}

// IsVf reports whether devPci is an SR-IOV VF (has a physfn symlink).
// Returns an error if the device itself does not exist in sysfs, so a
// mistyped PCI address is not silently classified as a PF.
func (o *realSysfs) IsVf(devPci string) (bool, error) {
	devDir := filepath.Join(o.basePath, "bus/pci/devices", devPci)
	if _, err := os.Stat(devDir); err != nil {
		return false, fmt.Errorf("PCI device %s not found in sysfs: %w", devPci, err)
	}
	// Lstat: a dangling physfn symlink still identifies the device as a VF.
	if _, err := os.Lstat(filepath.Join(devDir, "physfn")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat physfn for %s: %w", devPci, err)
	}
	return true, nil
}

// GetPciClass returns the device's PCI class code as written in sysfs
// (e.g. "0x020000"). The class file reflects PCI config space and is
// readable regardless of the bound driver (including vfio-pci).
func (o *realSysfs) GetPciClass(devPci string) (string, error) {
	classPath := filepath.Join(o.basePath, "bus/pci/devices", devPci, "class")
	data, err := os.ReadFile(classPath)
	if err != nil {
		return "", fmt.Errorf("failed to read PCI class for %s: %w", devPci, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// GetPfPci resolves the PF PCI address from a VF PCI address.
func (o *realSysfs) GetPfPci(vfPci string) (string, error) {
	physfn := filepath.Join(o.basePath, "bus/pci/devices", vfPci, "physfn")
	target, err := os.Readlink(physfn)
	if err != nil {
		return "", fmt.Errorf("failed to read physfn for VF %s: %w", vfPci, err)
	}
	return filepath.Base(target), nil
}

// GetPfName returns the network interface name of the PF.
//
// In switchdev mode the PF's PCI device owns several netdevs: its own
// uplink netdev plus one representor per VF, per SF and — on a DPU — per
// host PF. The uplink is the one whose phys_port_name has the PHYSICAL
// flavour ("p0"); rejecting only VF representors is not enough, because an
// SF or host-PF representor would then be mistaken for the PF.
func (o *realSysfs) GetPfName(pfPci string) (string, error) {
	netDir := filepath.Join(o.basePath, "bus/pci/devices", pfPci, "net")
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return "", fmt.Errorf("failed to read net dir for PF %s: %w", pfPci, err)
	}

	var fallback string
	for _, e := range entries {
		portName := o.readFileContent(filepath.Join(netDir, e.Name(), "phys_port_name"))
		switch {
		case uplinkPortNameRegex.MatchString(portName):
			return e.Name(), nil
		case portName == "":
			// Legacy mode (and drivers without devlink ports) expose no
			// phys_port_name at all. Remember it, but keep looking for an
			// explicit uplink port first.
			if fallback == "" {
				fallback = e.Name()
			}
		case !pciRepPortNameRegex.MatchString(portName):
			// Unknown, non-representor naming: better than nothing.
			if fallback == "" {
				fallback = e.Name()
			}
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	if len(entries) > 0 {
		return "", fmt.Errorf("no uplink netdev found for PF %s: all %d netdevs are representors", pfPci, len(entries))
	}
	return "", fmt.Errorf("no network device found for PF %s", pfPci)
}

// GetVfIndex finds the VF index by enumerating virtfn* symlinks under the PF.
func (o *realSysfs) GetVfIndex(pfPci, vfPci string) (int, error) {
	pfDir := filepath.Join(o.basePath, "bus/pci/devices", pfPci)
	entries, err := os.ReadDir(pfDir)
	if err != nil {
		return -1, fmt.Errorf("failed to read PF dir %s: %w", pfPci, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "virtfn") {
			continue
		}
		target, err := os.Readlink(filepath.Join(pfDir, e.Name()))
		if err != nil {
			continue
		}
		if filepath.Base(target) == vfPci {
			idx, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "virtfn"))
			if err != nil {
				continue
			}
			return idx, nil
		}
	}
	return -1, fmt.Errorf("VF %s not found among PF %s virtfn links", vfPci, pfPci)
}

// GetDriver returns the driver name bound to the given PCI device.
func (o *realSysfs) GetDriver(devPci string) (string, error) {
	driverLink := filepath.Join(o.basePath, "bus/pci/devices", devPci, "driver")
	target, err := os.Readlink(driverLink)
	if err != nil {
		return "", fmt.Errorf("failed to read driver for %s: %w", devPci, err)
	}
	return filepath.Base(target), nil
}

// GetVendorID returns the device's PCI vendor id without the 0x prefix
// (e.g. "15b3"), readable regardless of the bound driver.
func (o *realSysfs) GetVendorID(devPci string) (string, error) {
	return o.readPciID(devPci, "vendor")
}

// GetDeviceID returns the device's PCI device id without the 0x prefix.
func (o *realSysfs) GetDeviceID(devPci string) (string, error) {
	return o.readPciID(devPci, "device")
}

func (o *realSysfs) readPciID(devPci, file string) (string, error) {
	p := filepath.Join(o.basePath, "bus/pci/devices", devPci, file)
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("failed to read %s for %s: %w", file, devPci, err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(data)), "0x"), nil
}

// GetIOMMUGroup returns the IOMMU group number of the device.
func (o *realSysfs) GetIOMMUGroup(devPci string) (int, error) {
	groupLink := filepath.Join(o.basePath, "bus/pci/devices", devPci, "iommu_group")
	target, err := os.Readlink(groupLink)
	if err != nil {
		return -1, fmt.Errorf("failed to read iommu_group for %s: %w", devPci, err)
	}
	group, err := strconv.Atoi(filepath.Base(target))
	if err != nil {
		return -1, fmt.Errorf("failed to parse iommu group %q for %s: %w", filepath.Base(target), devPci, err)
	}
	return group, nil
}

// ListIOMMUGroupDevices lists the PCI addresses of every device in the group.
func (o *realSysfs) ListIOMMUGroupDevices(group int) ([]string, error) {
	groupDir := filepath.Join(o.basePath, "kernel/iommu_groups", strconv.Itoa(group), "devices")
	entries, err := os.ReadDir(groupDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read iommu group %d devices: %w", group, err)
	}
	devices := make([]string, 0, len(entries))
	for _, e := range entries {
		devices = append(devices, e.Name())
	}
	return devices, nil
}

// GetNumaNode returns the device's NUMA node, or -1 when the platform
// reports none.
func (o *realSysfs) GetNumaNode(devPci string) (int, error) {
	p := filepath.Join(o.basePath, "bus/pci/devices", devPci, "numa_node")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return -1, fmt.Errorf("failed to read numa_node for %s: %w", devPci, err)
	}
	node, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1, fmt.Errorf("failed to parse numa_node for %s: %w", devPci, err)
	}
	return node, nil
}

// GetRepresentor finds the VF representor netdev name for the VF at
// vfIndex under the eswitch port identified by port. The representor must
// belong to the same PCI controller as the uplink, so that a host-side VF
// representor on a DPU is never mistaken for a local one.
func (o *realSysfs) GetRepresentor(pfPci string, port PortID, vfIndex int) (string, error) {
	netDir := filepath.Join(o.basePath, "bus/pci/devices", pfPci, "net")
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return "", fmt.Errorf("failed to read net dir for PF %s: %w", pfPci, err)
	}
	for _, e := range entries {
		portName := o.readFileContent(filepath.Join(netDir, e.Name(), "phys_port_name"))
		matches := vfRepPortNameRegex.FindStringSubmatch(portName)
		if matches == nil {
			continue
		}
		if matches[1] != port.Controller {
			continue
		}
		pf, err := strconv.Atoi(matches[2])
		if err != nil {
			continue
		}
		vf, err := strconv.Atoi(matches[3])
		if err != nil {
			continue
		}
		if pf == port.PF && vf == vfIndex {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("representor not found for %svf%d under PF %s (are VFs created?)", port, vfIndex, pfPci)
}

// GetUplinkPort returns the eswitch port identity of the PF from its
// phys_port_name. The file is only populated for drivers that register
// devlink ports; an empty or unparsable value means the PF is not a
// switchdev uplink, which is a hard error on the switchdev path.
func (o *realSysfs) GetUplinkPort(pfName string) (PortID, error) {
	portNamePath := filepath.Join(o.basePath, "class/net", pfName, "phys_port_name")
	content := o.readFileContent(portNamePath)
	if content == "" {
		return PortID{}, fmt.Errorf("empty phys_port_name for %s (not a switchdev uplink?)", pfName)
	}

	if m := uplinkPortNameRegex.FindStringSubmatch(content); m != nil {
		idx, err := strconv.Atoi(m[2])
		if err != nil {
			return PortID{}, fmt.Errorf("failed to parse port number from phys_port_name %q: %w", content, err)
		}
		return PortID{Controller: m[1], PF: idx}, nil
	}

	// Some drivers report the uplink with the PCI_PF flavour ("pf0")
	// rather than PHYSICAL ("p0"); accept that spelling too.
	if m := pfPortNameRegex.FindStringSubmatch(content); m != nil {
		idx, err := strconv.Atoi(m[2])
		if err != nil {
			return PortID{}, fmt.Errorf("failed to parse PF number from phys_port_name %q: %w", content, err)
		}
		return PortID{Controller: m[1], PF: idx}, nil
	}

	return PortID{}, fmt.Errorf("unrecognized phys_port_name %q for %s, expected a physical port such as \"p0\"", content, pfName)
}

// GetBridgeVlanFiltering reads the bridge's vlan_filtering attribute.
// A read failure is reported rather than swallowed: treating an
// unreadable attribute as "filtering disabled" would turn a missing or
// non-bridge device into a misleading "VLAN requested but vlan_filtering
// is disabled" error.
func (o *realSysfs) GetBridgeVlanFiltering(bridge string) (bool, error) {
	p := filepath.Join(o.basePath, "class/net", bridge, "bridge", "vlan_filtering")
	data, err := os.ReadFile(p)
	if err != nil {
		return false, fmt.Errorf("failed to read vlan_filtering for %s (is it a bridge?): %w", bridge, err)
	}
	content := strings.TrimSpace(string(data))
	switch content {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("unexpected vlan_filtering value %q for bridge %s", content, bridge)
	}
}

// GetBridgeDefaultPvid reads the bridge's default_pvid attribute. A value
// of 0 means the bridge assigns no PVID to newly added ports.
func (o *realSysfs) GetBridgeDefaultPvid(bridge string) (int, error) {
	p := filepath.Join(o.basePath, "class/net", bridge, "bridge", "default_pvid")
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, fmt.Errorf("failed to read default_pvid for %s: %w", bridge, err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return 0, nil
	}
	pvid, err := strconv.Atoi(content)
	if err != nil {
		return 0, fmt.Errorf("failed to parse default_pvid %q for bridge %s: %w", content, bridge, err)
	}
	return pvid, nil
}

func (o *realSysfs) readFileContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
