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
	"net"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"k8s.io/klog/v2"
)

// Mode identifies how the allocated device is handled: eswitch legacy or
// eswitch switchdev (both Ethernet SR-IOV VFs), or passthrough — the device
// is handed through as-is (Ethernet PFs and all InfiniBand devices).
type Mode string

const (
	ModeLegacy      Mode = "legacy"
	ModeSwitchdev   Mode = "switchdev"
	ModePassthrough Mode = "passthrough"
)

// vfLinkStateAuto is IFLA_VF_LINK_STATE_AUTO: the VF follows the PF's
// link state (include/uapi/linux/if_link.h).
const vfLinkStateAuto uint32 = 0

// zeroMAC clears a VF's admin MAC, letting the driver fall back to the
// VF's own permanent address.
var zeroMAC = net.HardwareAddr{0, 0, 0, 0, 0, 0}

// HostState records the host-side configuration applied for one allocated
// device, persisted so that unprepare can restore what prepare changed. In
// passthrough mode only Mode and VfPci are populated (plus PfPci for an
// InfiniBand VF): VfPci holds the allocated device's own PCI address (JSON
// names kept compatible with the sriov-vfio CNI's cache files).
type HostState struct {
	Mode    Mode   `json:"mode"`
	VfPci   string `json:"vfPci"`
	PfPci   string `json:"pfPci"`
	PfName  string `json:"pfName"`
	VfIndex int    `json:"vfIndex"`
	Vlan    int    `json:"vlan"`

	// Switchdev-only fields
	Representor   string `json:"representor,omitempty"`
	Bridge        string `json:"bridge,omitempty"`
	Uplink        string `json:"uplink,omitempty"`
	VlanFiltering bool   `json:"vlanFiltering,omitempty"`

	// Original VF state, captured before prepare applies anything. Used
	// both to roll back a failed prepare and to restore the VF on
	// unprepare, so a setting the host provisioning put in place survives
	// a pod's lifetime.
	OrigMAC       string `json:"origMac"`
	OrigVlan      int    `json:"origVlan"`
	OrigSpoofChk  bool   `json:"origSpoofchk"`
	OrigTrust     bool   `json:"origTrust"`
	OrigMinTxRate int    `json:"origMinTxRate"`
	OrigMaxTxRate int    `json:"origMaxTxRate"`
	OrigLinkState uint32 `json:"origLinkState,omitempty"`

	// OrigRepMTU is the representor's MTU before prepare changed it
	// (switchdev only), so rollback and unprepare can put it back.
	OrigRepMTU int `json:"origRepMtu,omitempty"`
}

// Configurer applies and restores the host-side configuration of allocated
// VFIO devices: nothing for passthrough devices, VF settings via the PF in
// legacy mode, and additionally the representor/bridge lifecycle in
// switchdev mode.
type Configurer struct {
	sysfs   SysfsOps
	netlink NetlinkOps

	// lockDir holds the per-bridge flock files serializing bridge-VLAN
	// edits (shared with a host-installed sriov-vfio CNI).
	lockDir string

	// acquireLock is overridable in tests.
	acquireLock func(dir, bridge string) (*Lock, error)
}

// NewConfigurer creates a Configurer.
func NewConfigurer(sysfs SysfsOps, nl NetlinkOps, lockDir string) *Configurer {
	return &Configurer{sysfs: sysfs, netlink: nl, lockDir: lockDir, acquireLock: AcquireLock}
}

// ValidateIOMMUGroup checks that the device's IOMMU group is viable for
// VFIO: every other endpoint in the group must be bound to vfio-pci, a
// stub driver, or no driver at all — mirroring the kernel's own
// vfio_dev_viable() rule. A sibling bound to a live host driver makes
// VFIO_GROUP_GET_DEVICE_FD fail at VM start; failing prepare instead gives
// the operator an actionable error.
func (c *Configurer) ValidateIOMMUGroup(group int, devPci string) error {
	members, err := c.sysfs.ListIOMMUGroupDevices(group)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member == devPci {
			continue
		}
		driver, err := c.sysfs.GetDriver(member)
		if err != nil {
			// No driver symlink: an unbound device is viable.
			continue
		}
		switch driver {
		case "vfio-pci", "pci-stub", "pcieport", "pci-pf-stub":
		default:
			return fmt.Errorf("IOMMU group %d is not viable for VFIO: sibling device %s is bound to %s", group, member, driver)
		}
	}
	return nil
}

// Apply validates the device and applies the host-side configuration for
// one allocated device, returning the state unprepare needs to restore it.
//
// prior, when non-nil, is the state captured by an earlier Apply for the
// same device (kubelet retry, driver restart, or a stale entry from a
// reallocated device). Its Orig* fields are reused instead of re-reading
// the kernel, which by then reports the previously applied values rather
// than the host's own configuration.
func (c *Configurer) Apply(spec Spec, params *ClaimParams, prior *HostState) (*HostState, error) {
	isVf, err := c.sysfs.IsVf(spec.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to detect device type for %s: %w", spec.PCIAddress, err)
	}
	class, err := c.sysfs.GetPciClass(spec.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to detect device class: %w", err)
	}
	if !isNetworkClass(class) {
		return nil, fmt.Errorf("device %s has PCI class %s, expected a network controller (0x02xx or 0x0c06xx)", spec.PCIAddress, class)
	}

	driver, err := c.sysfs.GetDriver(spec.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to read device driver: %w", err)
	}
	if driver != "vfio-pci" {
		return nil, fmt.Errorf("device %s is bound to %s, expected vfio-pci", spec.PCIAddress, driver)
	}

	if isVf && isEthernetClass(class) {
		return c.applyVf(spec, params, prior)
	}
	return c.applyPassthrough(spec, params, isVf, class)
}

// applyVf handles an Ethernet SR-IOV VF: resolve the parent PF, detect its
// eswitch mode, and apply VF settings via the legacy or switchdev path.
func (c *Configurer) applyVf(spec Spec, params *ClaimParams, prior *HostState) (*HostState, error) {
	pfPci, err := c.sysfs.GetPfPci(spec.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve PF for VF %s: %w", spec.PCIAddress, err)
	}
	pfName, err := c.sysfs.GetPfName(pfPci)
	if err != nil {
		return nil, fmt.Errorf("failed to get PF name for %s: %w", pfPci, err)
	}
	vfIndex, err := c.sysfs.GetVfIndex(pfPci, spec.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get VF index: %w", err)
	}

	mode, err := c.getEswitchMode(pfPci)
	if err != nil {
		return nil, fmt.Errorf("failed to detect eswitch mode: %w", err)
	}
	klog.V(2).Infof("detected eswitch mode %s for PF %s (VF index %d)", mode, pfName, vfIndex)

	state := &HostState{
		Mode:    mode,
		VfPci:   spec.PCIAddress,
		PfPci:   pfPci,
		PfName:  pfName,
		VfIndex: vfIndex,
	}
	if prior != nil && prior.VfPci == spec.PCIAddress {
		state.OrigMAC = prior.OrigMAC
		state.OrigVlan = prior.OrigVlan
		state.OrigSpoofChk = prior.OrigSpoofChk
		state.OrigTrust = prior.OrigTrust
		state.OrigMinTxRate = prior.OrigMinTxRate
		state.OrigMaxTxRate = prior.OrigMaxTxRate
		state.OrigLinkState = prior.OrigLinkState
		state.OrigRepMTU = prior.OrigRepMTU
	} else {
		vfInfo, err := c.netlink.LinkGetVfInfo(pfName, vfIndex)
		if err != nil {
			return nil, fmt.Errorf("failed to read current VF state: %w", err)
		}
		state.OrigMAC = vfInfo.Mac.String()
		state.OrigVlan = vfInfo.Vlan
		state.OrigSpoofChk = vfInfo.Spoofchk
		state.OrigTrust = vfInfo.Trust
		state.OrigMinTxRate = vfInfo.MinTxRate
		state.OrigMaxTxRate = vfInfo.MaxTxRate
		state.OrigLinkState = vfInfo.LinkState
	}

	if mode == ModeLegacy {
		if err := c.applyLegacy(params, pfName, vfIndex, state); err != nil {
			return nil, err
		}
	} else {
		if err := c.applySwitchdev(params, pfPci, pfName, vfIndex, state); err != nil {
			return nil, err
		}
	}
	return state, nil
}

// PCI class codes (PCI-SIG Code and ID Assignment,
// https://pcisig.com/specifications): 0x02xxxx = network controller
// (0x0200 Ethernet, 0x0207 InfiniBand), 0x0c06xx = legacy
// serial-bus/InfiniBand controller (older HCAs).
func isNetworkClass(class string) bool {
	return strings.HasPrefix(class, "0x02") || strings.HasPrefix(class, "0x0c06")
}

func isEthernetClass(class string) bool { return strings.HasPrefix(class, "0x0200") }

func isInfiniBandClass(class string) bool {
	return strings.HasPrefix(class, "0x0207") || strings.HasPrefix(class, "0x0c06")
}

// applyPassthrough hands the vfio-pci-bound device through as-is: every
// network device except Ethernet VFs. Only sysfs is read — no netdev,
// netlink, or devlink operations — so no netdev is required and InfiniBand
// devices behave exactly like Ethernet PFs.
func (c *Configurer) applyPassthrough(spec Spec, params *ClaimParams, isVf bool, class string) (*HostState, error) {
	kind := deviceKind(isVf, class)
	if set := params.setFields(); len(set) > 0 {
		return nil, fmt.Errorf("device %s (%s) is handed through without host-side configuration; options %v are only valid for Ethernet SR-IOV VFs", spec.PCIAddress, kind, set)
	}
	state := &HostState{Mode: ModePassthrough, VfPci: spec.PCIAddress}
	if isVf {
		pfPci, err := c.sysfs.GetPfPci(spec.PCIAddress)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve PF for VF %s: %w", spec.PCIAddress, err)
		}
		state.PfPci = pfPci
	}
	klog.V(2).Infof("passthrough device %s (%s)", spec.PCIAddress, kind)
	return state, nil
}

// deviceKind describes the allocated device for logs and error messages.
func deviceKind(isVf bool, class string) string {
	link := "network class " + class
	switch {
	case isInfiniBandClass(class):
		link = "InfiniBand"
	case isEthernetClass(class):
		link = "Ethernet"
	}
	if isVf {
		return link + " VF"
	}
	return link + " PF"
}

// applyLegacy applies VF settings in legacy eswitch mode.
func (c *Configurer) applyLegacy(params *ClaimParams, pfName string, vfIndex int, state *HostState) error {
	pfLink, err := c.netlink.LinkByName(pfName)
	if err != nil {
		return fmt.Errorf("failed to get PF link %s: %w", pfName, err)
	}

	var rollback rollbackStack

	// MAC
	if params.MAC != "" {
		mac, _ := net.ParseMAC(params.MAC) // already validated
		klog.V(4).Infof("setting admin MAC %s via PF %s for VF %d", params.MAC, pfName, vfIndex)
		if err := c.netlink.LinkSetVfHardwareAddr(pfLink, vfIndex, mac); err != nil {
			rollback.run()
			return fmt.Errorf("failed to set MAC: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetVfHardwareAddr(pfLink, vfIndex, origMAC(state))
		})
		// Verify admin MAC was applied
		vfInfo, err := c.netlink.LinkGetVfInfo(pfName, vfIndex)
		if err != nil {
			rollback.run()
			return fmt.Errorf("failed to read back VF info after MAC set: %w", err)
		}
		if vfInfo.Mac.String() != mac.String() {
			rollback.run()
			return fmt.Errorf("admin MAC not applied: expected %s, got %s", mac, vfInfo.Mac)
		}
	}

	// VLAN: always set, 0 when omitted.
	vlan := 0
	if params.Vlan != nil {
		vlan = *params.Vlan
	}
	if err := c.netlink.LinkSetVfVlan(pfLink, vfIndex, vlan); err != nil {
		rollback.run()
		return fmt.Errorf("failed to set VLAN: %w", err)
	}
	rollback.push(func() {
		_ = c.netlink.LinkSetVfVlan(pfLink, vfIndex, state.OrigVlan)
	})
	state.Vlan = vlan

	// Spoofchk
	if params.SpoofChk != "" {
		check := params.SpoofChk == "on"
		if err := c.netlink.LinkSetVfSpoofchk(pfLink, vfIndex, check); err != nil {
			rollback.run()
			return fmt.Errorf("failed to set spoofchk: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetVfSpoofchk(pfLink, vfIndex, state.OrigSpoofChk)
		})
	}

	// Trust
	if params.Trust != "" {
		trust := params.Trust == "on"
		if err := c.netlink.LinkSetVfTrust(pfLink, vfIndex, trust); err != nil {
			rollback.run()
			return fmt.Errorf("failed to set trust: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetVfTrust(pfLink, vfIndex, state.OrigTrust)
		})
	}

	// Rates
	minRate, maxRate := 0, 0
	if params.MinTxRate != nil {
		minRate = *params.MinTxRate
	}
	if params.MaxTxRate != nil {
		maxRate = *params.MaxTxRate
	}
	if minRate > 0 || maxRate > 0 {
		if err := c.netlink.LinkSetVfRate(pfLink, vfIndex, minRate, maxRate); err != nil {
			rollback.run()
			return fmt.Errorf("failed to set rates: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetVfRate(pfLink, vfIndex, state.OrigMinTxRate, state.OrigMaxTxRate)
		})
	}

	// Link state = auto (always). Not every driver implements
	// ndo_set_vf_link_state — rtnetlink returns -EOPNOTSUPP when it is
	// absent — so a failure here is logged rather than treated as fatal.
	if err := c.netlink.LinkSetVfState(pfLink, vfIndex, vfLinkStateAuto); err != nil {
		klog.Warningf("could not set VF %d link state to auto on PF %s: %v", vfIndex, pfName, err)
	}

	return nil
}

// applySwitchdev handles switchdev mode: representor lifecycle, bridge attachment, VLAN.
func (c *Configurer) applySwitchdev(params *ClaimParams, pfPci, pfName string, vfIndex int, state *HostState) error {
	var rollback rollbackStack

	// Identify the PF's eswitch port so the representor lookup matches
	// both the PF number and the PCI controller.
	port, err := c.sysfs.GetUplinkPort(pfName)
	if err != nil {
		return fmt.Errorf("failed to get eswitch port for PF %s: %w", pfName, err)
	}

	// Find representor
	rep, err := c.sysfs.GetRepresentor(pfPci, port, vfIndex)
	if err != nil {
		return fmt.Errorf("representor not found for VF %d on PF %s: %w (are VFs created?)", vfIndex, pfName, err)
	}
	state.Representor = rep
	klog.V(2).Infof("found representor %s (port %s)", rep, port.String())

	// Resolve uplink and bridge
	var uplink, bridge string
	if params.Bridge != "" {
		// Explicit bridge: skip auto-detection
		bridge = params.Bridge
		bridgeLookup, err := c.netlink.LinkByName(bridge)
		if err != nil {
			return fmt.Errorf("bridge %s not found: %w", bridge, err)
		}
		if bridgeLookup.Type() != "bridge" {
			return fmt.Errorf("%s is type %s, expected bridge", bridge, bridgeLookup.Type())
		}
		// No uplink — FRR manages the vxlan device
		uplink = ""
		klog.V(2).Infof("using explicit bridge %s", bridge)
	} else {
		// Auto-detect uplink and bridge
		uplink, err = c.detectUplink(pfName)
		if err != nil {
			return fmt.Errorf("failed to detect uplink: %w", err)
		}
		bridge, err = c.detectBridge(uplink)
		if err != nil {
			return fmt.Errorf("failed to detect bridge for uplink %s: %w (has provisioning run?)", uplink, err)
		}
		klog.V(2).Infof("detected uplink %s, bridge %s", uplink, bridge)
	}
	state.Uplink = uplink
	state.Bridge = bridge

	// Read bridge vlan_filtering
	vlanFiltering, err := c.sysfs.GetBridgeVlanFiltering(bridge)
	if err != nil {
		return fmt.Errorf("failed to read bridge vlan_filtering: %w", err)
	}
	state.VlanFiltering = vlanFiltering

	// Validate VLAN vs bridge filtering
	vlan := 0
	if params.Vlan != nil {
		vlan = *params.Vlan
	}
	if vlan != 0 && !vlanFiltering {
		return fmt.Errorf("VLAN %d requested but bridge %s has vlan_filtering disabled", vlan, bridge)
	}
	state.Vlan = vlan

	// The representor's VLAN has to be one the uplink carries — the uplink is
	// the bridge's only path to the fabric. Get it wrong in either direction
	// and the VF is black-holed while prepare reports success: a VID the
	// uplink does not carry fails br_vlan.c:br_allowed_egress on the way out,
	// and a port left with no VLAN at all fails __allowed_ingress on the way
	// in. Neither is logged by the kernel.
	//
	// An explicit bridge (params.bridge set, FRR-managed vxlan) has no
	// uplink to validate against and stays unchecked. Unused today.
	if vlanFiltering && uplink != "" {
		vlan, err = c.resolveUplinkVlan(uplink, vlan)
		if err != nil {
			return err
		}
		state.Vlan = vlan
	}

	// Attach representor to bridge
	repLink, err := c.netlink.LinkByName(rep)
	if err != nil {
		return fmt.Errorf("failed to get representor link %s: %w", rep, err)
	}
	bridgeLink, err := c.netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("failed to get bridge link %s: %w", bridge, err)
	}

	// A representor already enslaved to a different bridge belongs to
	// another attachment (or to host provisioning). Silently re-homing it
	// would black-hole that traffic, so refuse instead.
	if master := repLink.Attrs().MasterIndex; master != 0 && master != bridgeLink.Attrs().Index {
		name := fmt.Sprintf("index %d", master)
		if masterLink, err := c.netlink.LinkByIndex(master); err == nil {
			name = masterLink.Attrs().Name
		}
		return fmt.Errorf("representor %s is already enslaved to %s, expected bridge %s", rep, name, bridge)
	}

	// Set MTU: from uplink when auto-detected, from bridge when explicit
	mtuSource := bridgeLink
	if uplink != "" {
		mtuSource, err = c.netlink.LinkByName(uplink)
		if err != nil {
			return fmt.Errorf("failed to get uplink link %s: %w", uplink, err)
		}
	}
	// A retry pass keeps the originally captured MTU rather than recording
	// its own previously applied value.
	if state.OrigRepMTU == 0 {
		state.OrigRepMTU = repLink.Attrs().MTU
	}
	if mtu := mtuSource.Attrs().MTU; mtu != repLink.Attrs().MTU {
		if err := c.netlink.LinkSetMTU(repLink, mtu); err != nil {
			return fmt.Errorf("failed to set representor MTU: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetMTU(repLink, state.OrigRepMTU)
		})
	}

	// Set master = bridge
	if err := c.netlink.LinkSetMaster(repLink, bridgeLink); err != nil {
		rollback.run()
		return fmt.Errorf("failed to attach representor to bridge: %w", err)
	}
	rollback.push(func() {
		_ = c.netlink.LinkSetNoMaster(repLink)
	})

	// Apply VF settings via PF netlink (MAC, rates only in switchdev)
	pfLink, err := c.netlink.LinkByName(pfName)
	if err != nil {
		rollback.run()
		return fmt.Errorf("failed to get PF link: %w", err)
	}

	if params.MAC != "" {
		mac, _ := net.ParseMAC(params.MAC)

		klog.V(4).Infof("setting admin MAC %s via PF %s for VF %d", params.MAC, pfName, vfIndex)
		if err := c.netlink.LinkSetVfHardwareAddr(pfLink, vfIndex, mac); err != nil {
			rollback.run()
			return fmt.Errorf("failed to set admin MAC: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetVfHardwareAddr(pfLink, vfIndex, origMAC(state))
		})

		// Verify admin MAC was applied
		vfInfo, err := c.netlink.LinkGetVfInfo(pfName, vfIndex)
		if err != nil {
			rollback.run()
			return fmt.Errorf("failed to read back VF info after MAC set: %w", err)
		}
		if vfInfo.Mac.String() != mac.String() {
			rollback.run()
			return fmt.Errorf("admin MAC not applied: expected %s, got %s", mac, vfInfo.Mac)
		}
	}

	minRate, maxRate := 0, 0
	if params.MinTxRate != nil {
		minRate = *params.MinTxRate
	}
	if params.MaxTxRate != nil {
		maxRate = *params.MaxTxRate
	}
	if minRate > 0 || maxRate > 0 {
		if err := c.netlink.LinkSetVfRate(pfLink, vfIndex, minRate, maxRate); err != nil {
			rollback.run()
			return fmt.Errorf("failed to set rates: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetVfRate(pfLink, vfIndex, state.OrigMinTxRate, state.OrigMaxTxRate)
		})
	}

	// Spoofchk/trust/linkState are gated on legacy mode in the mlx5
	// driver (mlx5_eswitch_set_vport_spoofchk/_trust/_state return
	// -EOPNOTSUPP when esw->mode != MLX5_ESWITCH_LEGACY), so they are
	// skipped rather than attempted and reported as failures.
	if params.SpoofChk != "" {
		klog.V(4).Info("spoofchk not supported in switchdev mode, skipping")
	}
	if params.Trust != "" {
		klog.V(4).Info("trust not supported in switchdev mode, skipping")
	}

	// Configure the bridge port VLAN before bringing the representor up.
	// Joining a vlan_filtering bridge gives the port the bridge's
	// default_pvid (br_vlan.c:nbp_vlan_init), so a representor brought up
	// first would briefly carry traffic on the wrong VLAN.
	if vlanFiltering && vlan != 0 {
		if err := c.configureBridgeVlan(rep, bridge, vlan); err != nil {
			rollback.run()
			return err
		}
	}

	// Bring UP
	if err := c.netlink.LinkSetUp(repLink); err != nil {
		rollback.run()
		return fmt.Errorf("failed to bring representor up: %w", err)
	}

	return nil
}

// resolveUplinkVlan returns the VLAN to program on the representor, checked
// against what the uplink bridge port actually carries.
//
// want == 0 means the network specifies no VLAN. It resolves to the VID the
// uplink carries egress-untagged, which host provisioning programs explicitly
// on an access uplink — the switchport there adds and strips the real tag, so
// that VID is a bridge-internal label with no meaning on the wire. The
// bridge's default_pvid is 0 on both access and trunk bridges and is never
// consulted. A trunk uplink carries no untagged VID and so has nothing to
// resolve to.
//
// A non-zero want must be a VID the uplink is a member of. On an access uplink
// that is only the untagged VID: a single egress-untagged VID is all a port can
// have, so there is no second VLAN for a network to pick.
func (c *Configurer) resolveUplinkVlan(uplink string, want int) (int, error) {
	link, err := c.netlink.LinkByName(uplink)
	if err != nil {
		return 0, fmt.Errorf("failed to get uplink link %s: %w", uplink, err)
	}
	vlanMap, err := c.netlink.BridgeVlanList()
	if err != nil {
		return 0, fmt.Errorf("failed to list bridge VLANs: %w", err)
	}
	// A kernel interface index is an int32 by definition
	// (struct ifinfomsg.ifi_index), so this narrowing is exact.
	idx := int32(link.Attrs().Index) //nolint:gosec // ifindex is int32 in the kernel ABI
	carried := vlanMap[idx]

	if want != 0 {
		for _, vi := range carried {
			if int(vi.Vid) == want {
				return want, nil
			}
		}
		return 0, fmt.Errorf("VLAN %d requested but uplink %s does not carry it (carries %s)",
			want, uplink, vlanSummary(carried))
	}

	for _, vi := range carried {
		if vi.PortVID() && vi.EngressUntag() {
			klog.V(2).Infof("resolved untagged VLAN %d from uplink %s", vi.Vid, uplink)
			return int(vi.Vid), nil
		}
	}
	return 0, fmt.Errorf("this network specifies no VLAN, but uplink %s carries no untagged VLAN "+
		"(carries %s); a network on a trunk uplink must specify a VLAN", uplink, vlanSummary(carried))
}

// vlanSummary renders a bridge port's VLAN membership for error messages, so
// an operator sees what the uplink offers without going to the node.
func vlanSummary(vlans []*nl.BridgeVlanInfo) string {
	if len(vlans) == 0 {
		return "no VLANs"
	}
	parts := make([]string, 0, len(vlans))
	for _, vi := range vlans {
		s := strconv.Itoa(int(vi.Vid))
		if vi.PortVID() && vi.EngressUntag() {
			s += " untagged"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// configureBridgeVlan sets up the representor as access port (PVID + untagged).
// Trunk VLANs on the uplink are pre-provisioned on the host and never touched.
func (c *Configurer) configureBridgeVlan(rep, bridge string, vlan int) error {
	lk, err := c.acquireLock(c.lockDir, bridge)
	if err != nil {
		return fmt.Errorf("failed to acquire VLAN lock for bridge %s: %w", bridge, err)
	}
	defer func() { _ = lk.Release() }()

	vid, err := vlanID(vlan)
	if err != nil {
		return err
	}

	repLink, err := c.netlink.LinkByName(rep)
	if err != nil {
		return fmt.Errorf("failed to get representor link: %w", err)
	}

	// Add the VLAN as PVID + untagged first. br_vlan.c's __vlan_add_pvid
	// moves the port's PVID to the new VID as part of this call, so the
	// subsequent delete of the old default PVID is a no-op for the PVID
	// itself (__vlan_delete_pvid only clears a PVID that still matches).
	// Deleting first would instead leave a window with no PVID at all, in
	// which the bridge drops untagged ingress frames.
	if err := c.netlink.BridgeVlanAdd(repLink, vid, true, true, false, true); err != nil {
		return fmt.Errorf("failed to add VLAN %d on representor %s: %w", vlan, rep, err)
	}

	c.removeDefaultPvid(repLink, rep, bridge, vlan)

	klog.V(2).Infof("configured bridge VLAN %d on representor %s", vlan, rep)
	return nil
}

// removeDefaultPvid drops the bridge's default PVID from the representor's
// bridge port. The kernel added it automatically when the port joined the
// bridge (br_vlan.c:nbp_vlan_init). Every step is best effort: a bridge
// with default_pvid 0 assigns none, and the entry may already be gone.
func (c *Configurer) removeDefaultPvid(repLink netlink.Link, rep, bridge string, vlan int) {
	defaultPvid, err := c.sysfs.GetBridgeDefaultPvid(bridge)
	if err != nil {
		klog.V(4).Infof("could not read bridge %s default_pvid: %v", bridge, err)
		return
	}
	if defaultPvid == vlan {
		// It is the VLAN just configured; deleting it would strip it.
		return
	}
	oldVid, err := vlanID(defaultPvid)
	if err != nil {
		// default_pvid 0: the bridge assigns no PVID, nothing to remove.
		return
	}
	if err := c.netlink.BridgeVlanDel(repLink, oldVid, false, false, false, true); err != nil {
		klog.V(4).Infof("could not remove default PVID %d from representor %s: %v", defaultPvid, rep, err)
	}
}

// Restore undoes the host-side configuration recorded in state. It is
// shared by unprepare and the prepare path that fails after configuring
// the host. All errors are collected rather than returned early, so
// cleanup continues past individual failures.
func (c *Configurer) Restore(state *HostState) []error {
	switch state.Mode {
	case ModePassthrough:
		// passthrough device: no netlink/netdev state to clean up
		return nil
	case ModeLegacy:
		return c.restoreLegacy(state)
	case ModeSwitchdev:
		return c.restoreSwitchdev(state)
	default:
		return []error{fmt.Errorf("unknown recorded mode %q, skipping netlink cleanup", state.Mode)}
	}
}

// restoreLegacy restores the VF settings captured at prepare time.
// Restoring the recorded values rather than resetting to fixed defaults
// preserves whatever the host provisioning had configured before the pod
// ran.
func (c *Configurer) restoreLegacy(state *HostState) []error {
	if state.PfName == "" {
		return []error{fmt.Errorf("recorded state has no PF name, cannot reset VF %d", state.VfIndex)}
	}
	var errs []error
	pfLink, err := c.netlink.LinkByName(state.PfName)
	if err != nil {
		return []error{fmt.Errorf("failed to get PF link %s: %w", state.PfName, err)}
	}

	if err := c.netlink.LinkSetVfHardwareAddr(pfLink, state.VfIndex, origMAC(state)); err != nil {
		errs = append(errs, fmt.Errorf("restore MAC: %w", err))
	}
	if err := c.netlink.LinkSetVfVlan(pfLink, state.VfIndex, state.OrigVlan); err != nil {
		errs = append(errs, fmt.Errorf("restore VLAN: %w", err))
	}
	if err := c.netlink.LinkSetVfSpoofchk(pfLink, state.VfIndex, state.OrigSpoofChk); err != nil {
		errs = append(errs, fmt.Errorf("restore spoofchk: %w", err))
	}
	if err := c.netlink.LinkSetVfTrust(pfLink, state.VfIndex, state.OrigTrust); err != nil {
		errs = append(errs, fmt.Errorf("restore trust: %w", err))
	}
	if err := c.netlink.LinkSetVfRate(pfLink, state.VfIndex, state.OrigMinTxRate, state.OrigMaxTxRate); err != nil {
		errs = append(errs, fmt.Errorf("restore rates: %w", err))
	}
	if err := c.netlink.LinkSetVfState(pfLink, state.VfIndex, state.OrigLinkState); err != nil {
		errs = append(errs, fmt.Errorf("restore link state: %w", err))
	}
	return errs
}

// restoreSwitchdev cleans up switchdev mode: VF settings, VLAN, representor.
func (c *Configurer) restoreSwitchdev(state *HostState) []error {
	var errs []error

	// Restore VF settings via PF. Only MAC and rates are settable in
	// switchdev mode; spoofchk, trust and link state are legacy-only.
	if state.PfName == "" {
		errs = append(errs, fmt.Errorf("recorded state has no PF name, skipping VF reset"))
	} else if pfLink, err := c.netlink.LinkByName(state.PfName); err != nil {
		errs = append(errs, fmt.Errorf("failed to get PF link %s: %w", state.PfName, err))
	} else {
		if err := c.netlink.LinkSetVfHardwareAddr(pfLink, state.VfIndex, origMAC(state)); err != nil {
			errs = append(errs, fmt.Errorf("restore MAC: %w", err))
		}
		if err := c.netlink.LinkSetVfRate(pfLink, state.VfIndex, state.OrigMinTxRate, state.OrigMaxTxRate); err != nil {
			errs = append(errs, fmt.Errorf("restore rates: %w", err))
		}
	}

	// Remove VLAN from bridge port (if configured)
	if state.Vlan != 0 && state.VlanFiltering {
		if vlErr := c.removeBridgeVlan(state); vlErr != nil {
			errs = append(errs, vlErr)
		}
	}

	// Detach representor from bridge, bring it down and restore its MTU.
	if state.Representor != "" {
		repLink, err := c.netlink.LinkByName(state.Representor)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get representor link %s: %w", state.Representor, err))
		} else {
			if err := c.netlink.LinkSetNoMaster(repLink); err != nil {
				errs = append(errs, fmt.Errorf("detach representor: %w", err))
			}
			if err := c.netlink.LinkSetDown(repLink); err != nil {
				errs = append(errs, fmt.Errorf("bring representor down: %w", err))
			}
			if state.OrigRepMTU > 0 && state.OrigRepMTU != repLink.Attrs().MTU {
				if err := c.netlink.LinkSetMTU(repLink, state.OrigRepMTU); err != nil {
					errs = append(errs, fmt.Errorf("restore representor MTU: %w", err))
				}
			}
		}
	}

	return errs
}

// removeBridgeVlan removes the VLAN from the representor.
// Uplink VLANs are owned by host provisioning and are never modified here.
func (c *Configurer) removeBridgeVlan(state *HostState) error {
	lk, err := c.acquireLock(c.lockDir, state.Bridge)
	if err != nil {
		return fmt.Errorf("failed to acquire VLAN lock: %w", err)
	}
	defer func() { _ = lk.Release() }()

	vid, err := vlanID(state.Vlan)
	if err != nil {
		return fmt.Errorf("recorded VLAN for representor %s is unusable: %w", state.Representor, err)
	}

	repLink, err := c.netlink.LinkByName(state.Representor)
	if err != nil {
		return fmt.Errorf("representor %s not found: %w", state.Representor, err)
	}

	if err := c.netlink.BridgeVlanDel(repLink, vid, true, true, false, true); err != nil {
		klog.Warningf("failed to remove VLAN %d from representor %s: %v", state.Vlan, state.Representor, err)
	}

	return nil
}

// detectUplink returns the uplink interface name: the PF's bond master
// when the PF is enslaved to a bond, otherwise the PF itself.
func (c *Configurer) detectUplink(pfName string) (string, error) {
	pfLink, err := c.netlink.LinkByName(pfName)
	if err != nil {
		return "", fmt.Errorf("failed to get PF link %s: %w", pfName, err)
	}
	masterIdx := pfLink.Attrs().MasterIndex
	if masterIdx == 0 {
		return pfName, nil
	}
	masterLink, err := c.netlink.LinkByIndex(masterIdx)
	if err != nil {
		return "", fmt.Errorf("failed to get PF master link: %w", err)
	}
	if masterLink.Type() == "bond" {
		return masterLink.Attrs().Name, nil
	}
	// PF's master is the bridge directly
	return pfName, nil
}

// detectBridge returns the bridge name for the given uplink.
func (c *Configurer) detectBridge(uplink string) (string, error) {
	uplinkLink, err := c.netlink.LinkByName(uplink)
	if err != nil {
		return "", fmt.Errorf("failed to get uplink link %s: %w", uplink, err)
	}
	masterIdx := uplinkLink.Attrs().MasterIndex
	if masterIdx == 0 {
		return "", fmt.Errorf("uplink %s has no bridge master (has provisioning run?)", uplink)
	}
	masterLink, err := c.netlink.LinkByIndex(masterIdx)
	if err != nil {
		return "", fmt.Errorf("failed to get uplink master link: %w", err)
	}
	if masterLink.Type() != "bridge" {
		return "", fmt.Errorf("uplink %s master is %s (type %s), expected bridge",
			uplink, masterLink.Attrs().Name, masterLink.Type())
	}
	return masterLink.Attrs().Name, nil
}

// getEswitchMode queries devlink for the PF's eswitch mode.
func (c *Configurer) getEswitchMode(pfPci string) (Mode, error) {
	mode, err := c.netlink.DevLinkGetEswitchMode(pfPci)
	if err != nil {
		return "", fmt.Errorf("devlink eswitch query for %s: %w", pfPci, err)
	}
	switch mode {
	case "switchdev":
		return ModeSwitchdev, nil
	default:
		return ModeLegacy, nil
	}
}

// rollbackStack collects the undo steps for a partially applied prepare.
// Steps run in reverse order, so each one only has to undo the single
// operation that registered it.
type rollbackStack struct {
	fns []func()
}

func (r *rollbackStack) push(fn func()) { r.fns = append(r.fns, fn) }

func (r *rollbackStack) run() {
	for i := len(r.fns) - 1; i >= 0; i-- {
		r.fns[i]()
	}
}

// vlanID narrows a VLAN to the uint16 the netlink bridge API takes.
// Parameters are range-checked at parse time, but values read back from
// persisted state are not: a corrupt entry holding, say, 67639 would wrap
// to 2103 and delete a VLAN belonging to a different attachment.
func vlanID(vlan int) (uint16, error) {
	if vlan < 1 || vlan > 4094 {
		return 0, fmt.Errorf("VLAN ID %d out of range 1-4094", vlan)
	}
	return uint16(vlan), nil
}

// origMAC returns the admin MAC captured at prepare time, falling back to
// the all-zero address — which clears the admin MAC — when the state holds
// no parsable value.
func origMAC(state *HostState) net.HardwareAddr {
	mac, err := net.ParseMAC(state.OrigMAC)
	if err != nil {
		return zeroMAC
	}
	return mac
}
