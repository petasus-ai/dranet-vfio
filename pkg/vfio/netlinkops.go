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
	"encoding/binary"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// NetlinkOps abstracts netlink operations for testability.
type NetlinkOps interface {
	// Link operations
	LinkByName(name string) (netlink.Link, error)
	LinkByIndex(index int) (netlink.Link, error)
	LinkSetUp(link netlink.Link) error
	LinkSetDown(link netlink.Link) error
	LinkSetMaster(link netlink.Link, master netlink.Link) error
	LinkSetNoMaster(link netlink.Link) error
	LinkSetMTU(link netlink.Link, mtu int) error

	// VF operations via PF
	LinkSetVfHardwareAddr(link netlink.Link, vf int, addr net.HardwareAddr) error
	LinkSetVfVlan(link netlink.Link, vf int, vlan int) error
	LinkSetVfSpoofchk(link netlink.Link, vf int, check bool) error
	LinkSetVfTrust(link netlink.Link, vf int, state bool) error
	LinkSetVfRate(link netlink.Link, vf int, minRate, maxRate int) error
	LinkSetVfState(link netlink.Link, vf int, state uint32) error

	// Bridge VLAN operations
	BridgeVlanAdd(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error
	BridgeVlanDel(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error
	BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error)

	// Devlink operations
	DevLinkGetEswitchMode(pfPci string) (string, error)

	// VF info query
	LinkGetVfInfo(pfName string, vf int) (*VfInfo, error)
	// LinkGetVfIbGUIDs reads the administrative InfiniBand node/port GUID of
	// every VF of the PF netdev (rtnetlink IFLA_VF_IB_*_GUID), keyed by VF index.
	LinkGetVfIbGUIDs(pfName string) (map[int]VfIbGUID, error)
}

// VfIbGUID is one VF's administrative InfiniBand GUIDs as the PF reports them
// (all zeros = unprogrammed).
type VfIbGUID struct {
	NodeGUID [8]byte
	PortGUID [8]byte
}

// VfInfo holds current VF settings read from the kernel.
type VfInfo struct {
	Mac       net.HardwareAddr
	Vlan      int
	Spoofchk  bool
	Trust     bool
	MinTxRate int
	MaxTxRate int
	LinkState uint32
}

// NewNetlink returns the real netlink implementation.
func NewNetlink() NetlinkOps { return &realNetlink{} }

type realNetlink struct{}

func (o *realNetlink) LinkByName(name string) (netlink.Link, error) { return netlink.LinkByName(name) }
func (o *realNetlink) LinkByIndex(index int) (netlink.Link, error)  { return netlink.LinkByIndex(index) }
func (o *realNetlink) LinkSetUp(link netlink.Link) error            { return netlink.LinkSetUp(link) }
func (o *realNetlink) LinkSetDown(link netlink.Link) error          { return netlink.LinkSetDown(link) }
func (o *realNetlink) LinkSetMaster(link, master netlink.Link) error {
	return netlink.LinkSetMaster(link, master)
}
func (o *realNetlink) LinkSetNoMaster(link netlink.Link) error { return netlink.LinkSetNoMaster(link) }
func (o *realNetlink) LinkSetMTU(link netlink.Link, mtu int) error {
	return netlink.LinkSetMTU(link, mtu)
}

func (o *realNetlink) LinkSetVfHardwareAddr(link netlink.Link, vf int, addr net.HardwareAddr) error {
	return netlink.LinkSetVfHardwareAddr(link, vf, addr)
}

func (o *realNetlink) LinkSetVfVlan(link netlink.Link, vf, vlan int) error {
	return netlink.LinkSetVfVlan(link, vf, vlan)
}

func (o *realNetlink) LinkSetVfSpoofchk(link netlink.Link, vf int, check bool) error {
	return netlink.LinkSetVfSpoofchk(link, vf, check)
}

func (o *realNetlink) LinkSetVfTrust(link netlink.Link, vf int, state bool) error {
	return netlink.LinkSetVfTrust(link, vf, state)
}

func (o *realNetlink) LinkSetVfRate(link netlink.Link, vf, minRate, maxRate int) error {
	return netlink.LinkSetVfRate(link, vf, minRate, maxRate)
}

func (o *realNetlink) LinkSetVfState(link netlink.Link, vf int, state uint32) error {
	return netlink.LinkSetVfState(link, vf, state)
}

func (o *realNetlink) BridgeVlanAdd(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return netlink.BridgeVlanAdd(link, vid, pvid, untagged, self, master)
}

func (o *realNetlink) BridgeVlanDel(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return netlink.BridgeVlanDel(link, vid, pvid, untagged, self, master)
}

func (o *realNetlink) BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error) {
	return netlink.BridgeVlanList()
}

func (o *realNetlink) DevLinkGetEswitchMode(pfPci string) (string, error) {
	dev, err := netlink.DevLinkGetDeviceByName("pci", pfPci)
	if err != nil {
		return "", fmt.Errorf("devlink get device %s: %w", pfPci, err)
	}
	return dev.Attrs.Eswitch.Mode, nil
}

// LinkGetVfInfo reads the current settings of a VF from its PF.
//
// netlink.LinkByName requests IFLA_EXT_MASK=RTEXT_FILTER_VF, so the
// kernel fills in IFLA_VFINFO_LIST; the resulting slice is indexed by VF
// number. An empty slice means the PF exposes no VF info at all — most
// often because the caller is not running with CAP_NET_ADMIN.
func (o *realNetlink) LinkGetVfInfo(pfName string, vf int) (*VfInfo, error) {
	if vf < 0 {
		return nil, fmt.Errorf("invalid VF index %d for PF %s", vf, pfName)
	}
	link, err := netlink.LinkByName(pfName)
	if err != nil {
		return nil, fmt.Errorf("failed to get PF link %s: %w", pfName, err)
	}
	vfs := link.Attrs().Vfs
	if len(vfs) == 0 {
		return nil, fmt.Errorf("PF %s reports no VF info (missing CAP_NET_ADMIN, or SR-IOV not enabled?)", pfName)
	}
	if vf >= len(vfs) {
		return nil, fmt.Errorf("VF index %d out of range (PF %s has %d VFs)", vf, pfName, len(vfs))
	}
	vi := vfs[vf]
	info := &VfInfo{
		Mac:       vi.Mac,
		Vlan:      vi.Vlan,
		Spoofchk:  vi.Spoofchk,
		MinTxRate: int(vi.MinTxRate),
		MaxTxRate: int(vi.MaxTxRate),
		LinkState: vi.LinkState,
	}
	info.Trust = vi.Trust != 0
	return info, nil
}

// LinkGetVfIbGUIDs dumps the PF's VF info list with the VF filter and picks
// the IFLA_VF_IB_NODE_GUID / IFLA_VF_IB_PORT_GUID attributes the upstream
// mlx5 driver reports (the sysfs sriov/<idx> attributes exist only with OFED).
func (o *realNetlink) LinkGetVfIbGUIDs(pfName string) (map[int]VfIbGUID, error) {
	req := nl.NewNetlinkRequest(unix.RTM_GETLINK, 0)
	req.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))
	req.AddData(nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(pfName)))
	req.AddData(nl.NewRtAttr(unix.IFLA_EXT_MASK, nl.Uint32Attr(nl.RTEXT_FILTER_VF)))
	msgs, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return nil, fmt.Errorf("getlink %s: %w", pfName, err)
	}
	out := map[int]VfIbGUID{}
	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[unix.SizeofIfInfomsg:])
		if err != nil {
			return nil, err
		}
		for _, a := range attrs {
			if a.Attr.Type != unix.IFLA_VFINFO_LIST {
				continue
			}
			vfs, err := nl.ParseRouteAttr(a.Value)
			if err != nil {
				return nil, err
			}
			for _, vf := range vfs {
				fields, err := nl.ParseRouteAttr(vf.Value)
				if err != nil {
					return nil, err
				}
				idx, g := -1, VfIbGUID{}
				for _, f := range fields {
					switch f.Attr.Type {
					case nl.IFLA_VF_MAC:
						idx = int(binary.LittleEndian.Uint32(f.Value[0:4]))
					case nl.IFLA_VF_IB_NODE_GUID:
						binary.BigEndian.PutUint64(g.NodeGUID[:], nl.DeserializeVfGUID(f.Value).GUID)
					case nl.IFLA_VF_IB_PORT_GUID:
						binary.BigEndian.PutUint64(g.PortGUID[:], nl.DeserializeVfGUID(f.Value).GUID)
					}
				}
				if idx >= 0 {
					out[idx] = g
				}
			}
		}
	}
	return out, nil
}
