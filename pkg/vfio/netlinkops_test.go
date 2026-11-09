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
	"reflect"
	"testing"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// vfInfoEntry builds one IFLA_VF_INFO as rtnl_fill_vfinfo does, the VF
// number in IFLA_VF_MAC and IFLA_VF_RATE.
func vfInfoEntry(macVf, rateVf, maxTxRate uint32, counters map[int]uint64) *nl.RtAttr {
	entry := nl.NewRtAttr(nl.IFLA_VF_INFO, nil)
	mac := nl.VfMac{Vf: macVf}
	entry.AddRtAttr(nl.IFLA_VF_MAC, mac.Serialize())
	rate := nl.VfRate{Vf: rateVf, MaxTxRate: maxTxRate}
	entry.AddRtAttr(nl.IFLA_VF_RATE, rate.Serialize())
	stats := entry.AddRtAttr(nl.IFLA_VF_STATS, nil)
	for _, typ := range []int{
		unix.IFLA_VF_STATS_RX_PACKETS, unix.IFLA_VF_STATS_TX_PACKETS, unix.IFLA_VF_STATS_RX_BYTES,
		unix.IFLA_VF_STATS_TX_BYTES, unix.IFLA_VF_STATS_BROADCAST, unix.IFLA_VF_STATS_MULTICAST,
	} {
		stats.AddRtAttr(typ, nl.Uint64Attr(counters[typ]))
	}
	// A 64-bit pad where the architecture needs one.
	stats.AddRtAttr(unix.IFLA_VF_STATS_PAD, nil)
	stats.AddRtAttr(unix.IFLA_VF_STATS_RX_DROPPED, nl.Uint64Attr(counters[unix.IFLA_VF_STATS_RX_DROPPED]))
	stats.AddRtAttr(unix.IFLA_VF_STATS_TX_DROPPED, nl.Uint64Attr(counters[unix.IFLA_VF_STATS_TX_DROPPED]))
	return entry
}

func linkMessage(operState uint8, flags uint32, entries ...*nl.RtAttr) []byte {
	info := nl.NewIfInfomsg(unix.AF_UNSPEC)
	info.Flags = flags
	msg := info.Serialize()
	msg = append(msg, nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated("enp1s0f1np1")).Serialize()...)
	msg = append(msg, nl.NewRtAttr(unix.IFLA_OPERSTATE, nl.Uint8Attr(operState)).Serialize()...)
	list := nl.NewRtAttr(unix.IFLA_VFINFO_LIST, nil)
	for _, e := range entries {
		list.AddChild(e)
	}
	return append(msg, list.Serialize()...)
}

// The kernel leaves out a VF whose configuration it cannot read, so the
// entries are keyed by the VF number they carry, not by position; an entry
// naming two VFs, or a VF number seen twice, counts as missing. The drop
// counters follow the kernel's numbering, past IFLA_VF_STATS_PAD.
func TestParseVfStats(t *testing.T) {
	counters := func(base uint64) map[int]uint64 {
		return map[int]uint64{
			unix.IFLA_VF_STATS_RX_PACKETS: base + 1, unix.IFLA_VF_STATS_TX_PACKETS: base + 2,
			unix.IFLA_VF_STATS_RX_BYTES: base + 3, unix.IFLA_VF_STATS_TX_BYTES: base + 4,
			unix.IFLA_VF_STATS_BROADCAST: base + 5, unix.IFLA_VF_STATS_MULTICAST: base + 6,
			unix.IFLA_VF_STATS_RX_DROPPED: base + 7, unix.IFLA_VF_STATS_TX_DROPPED: base + 8,
		}
	}
	msg := linkMessage(6, unix.IFF_UP|unix.IFF_LOWER_UP,
		vfInfoEntry(0, 0, 0, counters(0)),
		// VF 1 is missing; VF 2 comes second.
		vfInfoEntry(2, 2, 1000, counters(20)),
		vfInfoEntry(3, 4, 0, counters(30)),
		vfInfoEntry(5, 5, 0, counters(50)),
		vfInfoEntry(5, 5, 0, counters(51)),
	)
	got, err := parseVfStats(msg)
	if err != nil {
		t.Fatalf("parseVfStats: %v", err)
	}
	want := &PfVfStats{
		OperState: 6,
		Flags:     unix.IFF_UP | unix.IFF_LOWER_UP,
		VFs: map[int]VfStats{
			0: {RxPackets: 1, TxPackets: 2, RxBytes: 3, TxBytes: 4, Broadcast: 5, Multicast: 6, RxDropped: 7, TxDropped: 8},
			2: {RxPackets: 21, TxPackets: 22, RxBytes: 23, TxBytes: 24, Broadcast: 25, Multicast: 26, RxDropped: 27, TxDropped: 28, MaxTxRate: 1000},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseVfStats =\n%+v\nwant\n%+v", got, want)
	}

	if _, err := parseVfStats(msg[:unix.SizeofIfInfomsg-1]); err == nil {
		t.Error("a short message parsed")
	}
}
