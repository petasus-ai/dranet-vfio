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
	"slices"
	"sort"
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
// InfiniBand VF): VfPci holds the allocated device's own PCI address.
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
	// edits, also across driver restarts.
	lockDir string

	// acquireLock takes the per-bridge lock in lockDir.
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

// Plan validates the device and resolves, without changing the host, the
// configuration Apply programs and the originals Restore puts back. The
// caller persists the state before Apply, so a driver that dies inside Apply
// still finds the host's originals on its retry.
//
// prior, when non-nil, is the state an earlier prepare recorded for the same
// device (kubelet retry, driver restart, or a stale entry from a reallocated
// device). Its Orig* fields are reused instead of re-reading the kernel,
// which by then reports the previously applied values rather than the host's
// own configuration.
func (c *Configurer) Plan(spec Spec, params *ClaimParams, prior *HostState) (*HostState, error) {
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
		return c.planVf(spec, params, prior)
	}
	return c.planPassthrough(spec, params, isVf, class)
}

// planVf resolves an Ethernet SR-IOV VF: its parent PF, the PF's eswitch
// mode, the originals, and in switchdev mode the representor and bridge.
func (c *Configurer) planVf(spec Spec, params *ClaimParams, prior *HostState) (*HostState, error) {
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
	if params.Vlan != nil {
		state.Vlan = *params.Vlan
	}
	if prior != nil && prior.VfPci == spec.PCIAddress {
		state.OrigMAC = prior.OrigMAC
		state.OrigVlan = prior.OrigVlan
		state.OrigLinkState = prior.OrigLinkState
		state.OrigRepMTU = prior.OrigRepMTU
	} else {
		vfInfo, err := c.netlink.LinkGetVfInfo(pfName, vfIndex)
		if err != nil {
			return nil, fmt.Errorf("failed to read current VF state: %w", err)
		}
		state.OrigMAC = vfInfo.Mac.String()
		state.OrigVlan = vfInfo.Vlan
		state.OrigLinkState = vfInfo.LinkState
	}
	if mode == ModeSwitchdev {
		if err := c.planSwitchdev(state); err != nil {
			return nil, err
		}
	}
	return state, nil
}

// Apply programs the host as state plans it. A failure undoes what Apply
// changed before it returns.
func (c *Configurer) Apply(state *HostState, params *ClaimParams) error {
	switch state.Mode {
	case ModePassthrough:
		return nil
	case ModeLegacy:
		return c.applyLegacy(state, params)
	case ModeSwitchdev:
		return c.applySwitchdev(state, params)
	default:
		return fmt.Errorf("unknown mode %q", state.Mode)
	}
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

// planPassthrough hands the vfio-pci-bound device through as-is: every
// network device except Ethernet VFs. Only sysfs is read — no netdev,
// netlink, or devlink operations — so no netdev is required and InfiniBand
// devices behave exactly like Ethernet PFs.
func (c *Configurer) planPassthrough(spec Spec, params *ClaimParams, isVf bool, class string) (*HostState, error) {
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

// applyLegacy sets the VF's admin MAC, VLAN and link state via the PF
// (legacy eswitch mode).
func (c *Configurer) applyLegacy(state *HostState, params *ClaimParams) error {
	pfLink, err := c.netlink.LinkByName(state.PfName)
	if err != nil {
		return fmt.Errorf("failed to get PF link %s: %w", state.PfName, err)
	}

	var rollback rollbackStack
	if err := c.setVfMAC(pfLink, state, params, &rollback); err != nil {
		rollback.run()
		return err
	}

	// VLAN: always set, 0 when omitted.
	if err := c.netlink.LinkSetVfVlan(pfLink, state.VfIndex, state.Vlan); err != nil {
		rollback.run()
		return fmt.Errorf("failed to set VLAN: %w", err)
	}

	// Link state = auto (always). Not every driver implements
	// ndo_set_vf_link_state — rtnetlink returns -EOPNOTSUPP when it is
	// absent — so a failure here is logged rather than treated as fatal.
	if err := c.netlink.LinkSetVfState(pfLink, state.VfIndex, vfLinkStateAuto); err != nil {
		klog.Warningf("could not set VF %d link state to auto on PF %s: %v", state.VfIndex, state.PfName, err)
	}
	return nil
}

// setVfMAC sets the claim's admin MAC on the VF via the PF and reads it back.
func (c *Configurer) setVfMAC(pfLink netlink.Link, state *HostState, params *ClaimParams, rollback *rollbackStack) error {
	if params.MAC == "" {
		return nil
	}
	mac, _ := net.ParseMAC(params.MAC) // already validated
	klog.V(4).Infof("setting admin MAC %s via PF %s for VF %d", params.MAC, state.PfName, state.VfIndex)
	if err := c.netlink.LinkSetVfHardwareAddr(pfLink, state.VfIndex, mac); err != nil {
		return fmt.Errorf("failed to set admin MAC: %w", err)
	}
	rollback.push(func() {
		_ = c.netlink.LinkSetVfHardwareAddr(pfLink, state.VfIndex, origMAC(state))
	})
	vfInfo, err := c.netlink.LinkGetVfInfo(state.PfName, state.VfIndex)
	if err != nil {
		return fmt.Errorf("failed to read back VF info after MAC set: %w", err)
	}
	if vfInfo.Mac.String() != mac.String() {
		return fmt.Errorf("admin MAC not applied: expected %s, got %s", mac, vfInfo.Mac)
	}
	return nil
}

// planSwitchdev resolves the switchdev attachment: the VF representor, the
// uplink (PF or its bond) and its bridge, and the VLAN the representor gets.
func (c *Configurer) planSwitchdev(state *HostState) error {
	// Identify the PF's eswitch port so the representor lookup matches
	// both the PF number and the PCI controller.
	port, err := c.sysfs.GetUplinkPort(state.PfName)
	if err != nil {
		return fmt.Errorf("failed to get eswitch port for PF %s: %w", state.PfName, err)
	}
	rep, err := c.sysfs.GetRepresentor(state.PfPci, port, state.VfIndex)
	if err != nil {
		return fmt.Errorf("representor not found for VF %d on PF %s: %w (are VFs created?)", state.VfIndex, state.PfName, err)
	}
	state.Representor = rep
	klog.V(2).Infof("found representor %s (port %s)", rep, port.String())

	uplink, err := c.detectUplink(state.PfName)
	if err != nil {
		return fmt.Errorf("failed to detect uplink: %w", err)
	}
	bridge, err := c.detectBridge(uplink)
	if err != nil {
		return fmt.Errorf("failed to detect bridge for uplink %s: %w (has provisioning run?)", uplink, err)
	}
	klog.V(2).Infof("detected uplink %s, bridge %s", uplink, bridge)
	state.Uplink = uplink
	state.Bridge = bridge

	vlanFiltering, err := c.sysfs.GetBridgeVlanFiltering(bridge)
	if err != nil {
		return fmt.Errorf("failed to read bridge vlan_filtering: %w", err)
	}
	state.VlanFiltering = vlanFiltering
	if state.Vlan != 0 && !vlanFiltering {
		return fmt.Errorf("VLAN %d requested but bridge %s has vlan_filtering disabled", state.Vlan, bridge)
	}

	// The representor's VLAN has to be one the uplink carries — the uplink is
	// the bridge's only path to the fabric. Get it wrong in either direction
	// and the VF is black-holed while prepare reports success: a VID the
	// uplink does not carry fails br_vlan.c:br_allowed_egress on the way out,
	// and a port left with no VLAN at all fails __allowed_ingress on the way
	// in. Neither is logged by the kernel.
	if vlanFiltering {
		if state.Vlan, err = c.resolveUplinkVlan(uplink, state.Vlan); err != nil {
			return err
		}
	}

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
	// A retry keeps the originally captured MTU rather than recording its
	// own previously applied value.
	if state.OrigRepMTU == 0 {
		state.OrigRepMTU = repLink.Attrs().MTU
	}
	return nil
}

// applySwitchdev attaches the representor to the bridge with the uplink's
// MTU and its access VLAN, sets the admin MAC via the PF, and brings the
// representor up.
func (c *Configurer) applySwitchdev(state *HostState, params *ClaimParams) error {
	repLink, err := c.netlink.LinkByName(state.Representor)
	if err != nil {
		return fmt.Errorf("failed to get representor link %s: %w", state.Representor, err)
	}
	bridgeLink, err := c.netlink.LinkByName(state.Bridge)
	if err != nil {
		return fmt.Errorf("failed to get bridge link %s: %w", state.Bridge, err)
	}
	uplinkLink, err := c.netlink.LinkByName(state.Uplink)
	if err != nil {
		return fmt.Errorf("failed to get uplink link %s: %w", state.Uplink, err)
	}
	pfLink, err := c.netlink.LinkByName(state.PfName)
	if err != nil {
		return fmt.Errorf("failed to get PF link %s: %w", state.PfName, err)
	}

	var rollback rollbackStack
	if mtu := uplinkLink.Attrs().MTU; mtu != repLink.Attrs().MTU {
		if err := c.netlink.LinkSetMTU(repLink, mtu); err != nil {
			return fmt.Errorf("failed to set representor MTU: %w", err)
		}
		rollback.push(func() {
			_ = c.netlink.LinkSetMTU(repLink, state.OrigRepMTU)
		})
	}

	if err := c.netlink.LinkSetMaster(repLink, bridgeLink); err != nil {
		rollback.run()
		return fmt.Errorf("failed to attach representor to bridge: %w", err)
	}
	rollback.push(func() {
		_ = c.netlink.LinkSetNoMaster(repLink)
	})

	if err := c.setVfMAC(pfLink, state, params, &rollback); err != nil {
		rollback.run()
		return err
	}

	// Configure the bridge port VLAN before bringing the representor up.
	// Joining a vlan_filtering bridge gives the port the bridge's
	// default_pvid (br_vlan.c:nbp_vlan_init), so a representor brought up
	// first would briefly carry traffic on the wrong VLAN.
	if state.VlanFiltering && state.Vlan != 0 {
		if err := c.configureBridgeVlan(state.Representor, state.Bridge, state.Vlan); err != nil {
			rollback.run()
			return err
		}
	}

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

	if vid, ok := untaggedVid(carried); ok {
		klog.V(2).Infof("resolved untagged VLAN %d from uplink %s", vid, uplink)
		return vid, nil
	}
	return 0, fmt.Errorf("this network specifies no VLAN, but uplink %s carries no untagged VLAN "+
		"(carries %s); a network on a trunk uplink must specify a VLAN", uplink, vlanSummary(carried))
}

// untaggedVid returns the VID a bridge port carries egress-untagged as its
// PVID — the VID an unspecified-VLAN network resolves to.
func untaggedVid(vlans []*nl.BridgeVlanInfo) (int, bool) {
	for _, vi := range vlans {
		if vi.PortVID() && vi.EngressUntag() {
			return int(vi.Vid), true
		}
	}
	return 0, false
}

// compressVlanRanges renders VIDs ascending with contiguous runs collapsed
// ("1,100-119,4000").
func compressVlanRanges(vlans []*nl.BridgeVlanInfo) string {
	vids := make([]int, 0, len(vlans))
	for _, vi := range vlans {
		vids = append(vids, int(vi.Vid))
	}
	sort.Ints(vids)
	parts := make([]string, 0, len(vids))
	for i := 0; i < len(vids); {
		j := i
		for j+1 < len(vids) && vids[j+1] == vids[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(vids[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", vids[i], vids[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// maxVlanAttrLen is the DRA cap on a string attribute value
// (DeviceAttributeMaxValueLength); the uplinkVlans attribute must fit it.
const maxVlanAttrLen = 64

// formatVlanRanges is compressVlanRanges capped for the uplinkVlans device
// attribute: on overflow trailing ranges are dropped and truncated is true.
// Truncation is only the attribute size limit — the uplink carries the
// dropped VLANs too — so a consumer treats unlisted VIDs as unknown, never as
// an error, and prepare stays the authority. A KEP-5491 list attribute
// (DRAListTypeAttributes, alpha in Kubernetes 1.36) would lift the limit.
func formatVlanRanges(vlans []*nl.BridgeVlanInfo) (string, bool) {
	out := compressVlanRanges(vlans)
	if len(out) <= maxVlanAttrLen {
		return out, false
	}
	cut := strings.LastIndex(out[:maxVlanAttrLen+1], ",")
	if cut < 0 {
		return "", true
	}
	return out[:cut], true
}

// formatVlanExact renders the full VID set as a comma-padded enumeration
// (",1,100,101,") for exact CEL membership tests
// (uplinkVlansExact.contains(",<vid>,")). ok is false when the enumeration
// does not fit the attribute cap — the device then publishes the
// uplinkVlansExactOverflow escape instead, consumers stay permissive and the
// prepare path remains the authority.
func formatVlanExact(vlans []*nl.BridgeVlanInfo) (string, bool) {
	vids := make([]int, 0, len(vlans))
	for _, vi := range vlans {
		vids = append(vids, int(vi.Vid))
	}
	sort.Ints(vids)
	var b strings.Builder
	b.WriteString(",")
	for _, vid := range vids {
		b.WriteString(strconv.Itoa(vid))
		b.WriteString(",")
	}
	out := b.String()
	if len(out) > maxVlanAttrLen {
		return "", false
	}
	return out, true
}

// vlanSummary renders a bridge port's VLAN membership for error messages, so
// an operator sees what the uplink offers without going to the node.
func vlanSummary(vlans []*nl.BridgeVlanInfo) string {
	if len(vlans) == 0 {
		return "no VLANs"
	}
	s := compressVlanRanges(vlans)
	if vid, ok := untaggedVid(vlans); ok {
		s += fmt.Sprintf(" (%d untagged)", vid)
	}
	return s
}

// configureBridgeVlan sets up the representor as access port (PVID + untagged)
// of exactly that VLAN. Trunk VLANs on the uplink are pre-provisioned on the
// host and never touched.
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
	// subsequent deletes leave the PVID alone (__vlan_delete_pvid only
	// clears a PVID that still matches). Deleting first would instead leave
	// a window with no PVID at all, in which the bridge drops untagged
	// ingress frames.
	if err := c.netlink.BridgeVlanAdd(repLink, vid, true, true, false, true); err != nil {
		return fmt.Errorf("failed to add VLAN %d on representor %s: %w", vlan, rep, err)
	}

	if err := c.removeOtherBridgeVlans(repLink, rep, vid); err != nil {
		return err
	}

	klog.V(2).Infof("configured bridge VLAN %d on representor %s", vlan, rep)
	return nil
}

// bridgeVlanDumpAttempts bounds the bridge VLAN dumps that may miss a port.
const bridgeVlanDumpAttempts = 3

// removeOtherBridgeVlans deletes every VLAN of the representor's bridge port
// but keep: the bridge's default PVID the port got on joining
// (br_vlan.c:nbp_vlan_init), and the VLANs of an earlier claim when the VF
// was reallocated while its representor stayed enslaved — re-enslaving to the
// same bridge keeps them, and the VF would receive that VLAN's traffic. Only a
// dump listing keep on the port counts: the kernel pages bridge dumps by
// position, so a port removed meanwhile can shift this one out of it.
func (c *Configurer) removeOtherBridgeVlans(repLink netlink.Link, rep string, keep uint16) error {
	idx := int32(repLink.Attrs().Index) //nolint:gosec // ifindex is int32 in the kernel ABI
	for range bridgeVlanDumpAttempts {
		vlanMap, err := c.netlink.BridgeVlanList()
		if err != nil {
			return fmt.Errorf("failed to list bridge VLANs of representor %s: %w", rep, err)
		}
		vlans := vlanMap[idx]
		if !slices.ContainsFunc(vlans, func(vi *nl.BridgeVlanInfo) bool { return vi.Vid == keep }) {
			continue
		}
		for _, vi := range vlans {
			if vi.Vid == keep {
				continue
			}
			if err := c.netlink.BridgeVlanDel(repLink, vi.Vid, false, false, false, true); err != nil {
				return fmt.Errorf("failed to remove VLAN %d from representor %s: %w", vi.Vid, rep, err)
			}
		}
		return nil
	}
	return fmt.Errorf("no bridge VLAN dump listed VLAN %d on representor %s", keep, rep)
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
	if err := c.netlink.LinkSetVfState(pfLink, state.VfIndex, state.OrigLinkState); err != nil {
		errs = append(errs, fmt.Errorf("restore link state: %w", err))
	}
	return errs
}

// restoreSwitchdev cleans up switchdev mode: VF settings, VLAN, representor.
func (c *Configurer) restoreSwitchdev(state *HostState) []error {
	var errs []error

	// Restore the admin MAC via the PF; VLAN and link state are legacy-only
	// VF settings.
	if state.PfName == "" {
		errs = append(errs, fmt.Errorf("recorded state has no PF name, skipping VF reset"))
	} else if pfLink, err := c.netlink.LinkByName(state.PfName); err != nil {
		errs = append(errs, fmt.Errorf("failed to get PF link %s: %w", state.PfName, err))
	} else {
		if err := c.netlink.LinkSetVfHardwareAddr(pfLink, state.VfIndex, origMAC(state)); err != nil {
			errs = append(errs, fmt.Errorf("restore MAC: %w", err))
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
// when the PF is enslaved to a bond, otherwise the PF itself. Package-level
// so the inventory resolves the exact same uplink the prepare path uses.
func detectUplink(nlops NetlinkOps, pfName string) (string, error) {
	pfLink, err := nlops.LinkByName(pfName)
	if err != nil {
		return "", fmt.Errorf("failed to get PF link %s: %w", pfName, err)
	}
	masterIdx := pfLink.Attrs().MasterIndex
	if masterIdx == 0 {
		return pfName, nil
	}
	masterLink, err := nlops.LinkByIndex(masterIdx)
	if err != nil {
		return "", fmt.Errorf("failed to get PF master link: %w", err)
	}
	if masterLink.Type() == "bond" {
		return masterLink.Attrs().Name, nil
	}
	// PF's master is the bridge directly
	return pfName, nil
}

func (c *Configurer) detectUplink(pfName string) (string, error) {
	return detectUplink(c.netlink, pfName)
}

// detectBridge returns the bridge name for the given uplink.
func detectBridge(nlops NetlinkOps, uplink string) (string, error) {
	uplinkLink, err := nlops.LinkByName(uplink)
	if err != nil {
		return "", fmt.Errorf("failed to get uplink link %s: %w", uplink, err)
	}
	masterIdx := uplinkLink.Attrs().MasterIndex
	if masterIdx == 0 {
		return "", fmt.Errorf("uplink %s has no bridge master (has provisioning run?)", uplink)
	}
	masterLink, err := nlops.LinkByIndex(masterIdx)
	if err != nil {
		return "", fmt.Errorf("failed to get uplink master link: %w", err)
	}
	if masterLink.Type() != "bridge" {
		return "", fmt.Errorf("uplink %s master is %s (type %s), expected bridge",
			uplink, masterLink.Attrs().Name, masterLink.Type())
	}
	return masterLink.Attrs().Name, nil
}

func (c *Configurer) detectBridge(uplink string) (string, error) {
	return detectBridge(c.netlink, uplink)
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
