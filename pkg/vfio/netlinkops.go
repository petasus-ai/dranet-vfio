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
	"sigs.k8s.io/dranet/internal/nlwrap"
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
	LinkSetVfState(link netlink.Link, vf int, state uint32) error
	LinkSetVfRate(link netlink.Link, vf, minRate, maxRate int) error

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
	// LinkGetVfStats reads the PF netdev's link state and every VF's counters
	// and transmit cap in one RTM_GETLINK.
	LinkGetVfStats(pfName string) (*PfVfStats, error)
}

// PfVfStats is a PF netdev's link state and its VFs' statistics.
type PfVfStats struct {
	OperState uint8  // IFLA_OPERSTATE
	Flags     uint32 // ifi_flags
	// VFs by the VF number each entry carries; the kernel leaves out a VF
	// whose configuration it cannot read, so positions do not count.
	VFs map[int]VfStats
}

// VfStats is one VF's IFLA_VF_STATS (VF-relative: rx is what the VF
// received) and its settings.
type VfStats struct {
	RxBytes   uint64
	TxBytes   uint64
	RxPackets uint64
	TxPackets uint64
	Multicast uint64
	Broadcast uint64
	RxDropped uint64
	TxDropped uint64
	MinTxRate uint32 // Mbit/s, 0 = none
	MaxTxRate uint32 // Mbit/s, 0 = none
	Mac       net.HardwareAddr
	Vlan      int
	LinkState uint32
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
	LinkState uint32
	MinTxRate int // Mbit/s, 0 = none
	MaxTxRate int // Mbit/s, 0 = none
}

// NewNetlink returns the real netlink implementation.
func NewNetlink() NetlinkOps { return &realNetlink{} }

type realNetlink struct{}

func (o *realNetlink) LinkByName(name string) (netlink.Link, error) { return nlwrap.LinkByName(name) }
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

func (o *realNetlink) LinkSetVfState(link netlink.Link, vf int, state uint32) error {
	return netlink.LinkSetVfState(link, vf, state)
}

func (o *realNetlink) LinkSetVfRate(link netlink.Link, vf, minRate, maxRate int) error {
	return netlink.LinkSetVfRate(link, vf, minRate, maxRate)
}

func (o *realNetlink) BridgeVlanAdd(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return netlink.BridgeVlanAdd(link, vid, pvid, untagged, self, master)
}

func (o *realNetlink) BridgeVlanDel(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return netlink.BridgeVlanDel(link, vid, pvid, untagged, self, master)
}

func (o *realNetlink) BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error) {
	return nlwrap.BridgeVlanList()
}

func (o *realNetlink) DevLinkGetEswitchMode(pfPci string) (string, error) {
	dev, err := netlink.DevLinkGetDeviceByName("pci", pfPci)
	if err != nil {
		return "", fmt.Errorf("devlink get device %s: %w", pfPci, err)
	}
	return dev.Attrs.Eswitch.Mode, nil
}

// LinkGetVfInfo reads the current settings of a VF from its PF, by the VF
// number its entry carries.
func (o *realNetlink) LinkGetVfInfo(pfName string, vf int) (*VfInfo, error) {
	if vf < 0 {
		return nil, fmt.Errorf("invalid VF index %d for PF %s", vf, pfName)
	}
	stats, err := o.LinkGetVfStats(pfName)
	if err != nil {
		return nil, fmt.Errorf("failed to read the VFs of PF %s: %w", pfName, err)
	}
	if len(stats.VFs) == 0 {
		return nil, fmt.Errorf("PF %s reports no VF info (missing CAP_NET_ADMIN, or SR-IOV not enabled?)", pfName)
	}
	st, ok := stats.VFs[vf]
	if !ok {
		return nil, fmt.Errorf("PF %s reports no entry for VF %d", pfName, vf)
	}
	return &VfInfo{
		Mac:       st.Mac,
		Vlan:      st.Vlan,
		LinkState: st.LinkState,
		MinTxRate: int(st.MinTxRate),
		MaxTxRate: int(st.MaxTxRate),
	}, nil
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

func (o *realNetlink) LinkGetVfStats(pfName string) (*PfVfStats, error) {
	req := nl.NewNetlinkRequest(unix.RTM_GETLINK, 0)
	req.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))
	nameAttr := uint16(unix.IFLA_IFNAME)
	if len(pfName) > 15 {
		nameAttr = unix.IFLA_ALT_IFNAME
	}
	req.AddData(nl.NewRtAttr(int(nameAttr), nl.ZeroTerminated(pfName)))
	req.AddData(nl.NewRtAttr(unix.IFLA_EXT_MASK, nl.Uint32Attr(nl.RTEXT_FILTER_VF)))
	msgs, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if err != nil {
		return nil, fmt.Errorf("getlink %s: %w", pfName, err)
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("getlink %s: %d replies", pfName, len(msgs))
	}
	return parseVfStats(msgs[0])
}

// parseVfStats decodes an RTM_NEWLINK message. Each VF is keyed by the vf
// field of its IFLA_VF_MAC; an entry whose IFLA_VF_RATE names another VF,
// or a VF number seen twice, counts as missing.
func parseVfStats(msg []byte) (*PfVfStats, error) {
	if len(msg) < unix.SizeofIfInfomsg {
		return nil, fmt.Errorf("short link message (%d bytes)", len(msg))
	}
	out := &PfVfStats{Flags: nl.DeserializeIfInfomsg(msg).Flags, VFs: map[int]VfStats{}}
	attrs, err := nl.ParseRouteAttr(msg[unix.SizeofIfInfomsg:])
	if err != nil {
		return nil, err
	}
	native := nl.NativeEndian()
	duplicate := map[int]bool{}
	for _, a := range attrs {
		switch a.Attr.Type {
		case unix.IFLA_OPERSTATE:
			if len(a.Value) >= 1 {
				out.OperState = a.Value[0]
			}
		case unix.IFLA_VFINFO_LIST:
			entries, err := nl.ParseRouteAttr(a.Value)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Attr.Type != nl.IFLA_VF_INFO {
					continue
				}
				fields, err := nl.ParseRouteAttr(entry.Value)
				if err != nil {
					return nil, err
				}
				vf, rateVf := -1, -1
				var st VfStats
				for _, f := range fields {
					switch f.Attr.Type {
					case nl.IFLA_VF_MAC:
						if len(f.Value) >= nl.SizeofVfMac {
							mac := nl.DeserializeVfMac(f.Value)
							vf = int(mac.Vf)
							st.Mac = append(net.HardwareAddr(nil), mac.Mac[:6]...)
						}
					case nl.IFLA_VF_VLAN:
						if len(f.Value) >= nl.SizeofVfVlan {
							st.Vlan = int(nl.DeserializeVfVlan(f.Value).Vlan)
						}
					case nl.IFLA_VF_LINK_STATE:
						if len(f.Value) >= nl.SizeofVfLinkState {
							st.LinkState = nl.DeserializeVfLinkState(f.Value).LinkState
						}
					case nl.IFLA_VF_RATE:
						// struct ifla_vf_rate { vf, min_tx_rate, max_tx_rate }
						if len(f.Value) >= nl.SizeofVfRate {
							rateVf = int(native.Uint32(f.Value[0:4]))
							st.MinTxRate = native.Uint32(f.Value[4:8])
							st.MaxTxRate = native.Uint32(f.Value[8:12])
						}
					case nl.IFLA_VF_STATS:
						if err := parseVfCounters(f.Value, &st); err != nil {
							return nil, err
						}
					}
				}
				if vf < 0 || (rateVf >= 0 && rateVf != vf) {
					continue
				}
				if _, seen := out.VFs[vf]; seen || duplicate[vf] {
					duplicate[vf] = true
					delete(out.VFs, vf)
					continue
				}
				out.VFs[vf] = st
			}
		}
	}
	return out, nil
}

// parseVfCounters decodes IFLA_VF_STATS by the kernel's numbering
// (netlink v1.3.1 numbers the drop counters without IFLA_VF_STATS_PAD).
func parseVfCounters(data []byte, st *VfStats) error {
	attrs, err := nl.ParseRouteAttr(data)
	if err != nil {
		return err
	}
	native := nl.NativeEndian()
	for _, a := range attrs {
		if len(a.Value) != 8 {
			continue
		}
		v := native.Uint64(a.Value)
		switch a.Attr.Type {
		case unix.IFLA_VF_STATS_RX_PACKETS:
			st.RxPackets = v
		case unix.IFLA_VF_STATS_TX_PACKETS:
			st.TxPackets = v
		case unix.IFLA_VF_STATS_RX_BYTES:
			st.RxBytes = v
		case unix.IFLA_VF_STATS_TX_BYTES:
			st.TxBytes = v
		case unix.IFLA_VF_STATS_BROADCAST:
			st.Broadcast = v
		case unix.IFLA_VF_STATS_MULTICAST:
			st.Multicast = v
		case unix.IFLA_VF_STATS_RX_DROPPED:
			st.RxDropped = v
		case unix.IFLA_VF_STATS_TX_DROPPED:
			st.TxDropped = v
		}
	}
	return nil
}
