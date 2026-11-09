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

	mdnetlink "github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

type linkModeBit struct {
	index uint32
	name  string
	set   bool
}

// encodeLinkModes builds an ETHTOOL_MSG_LINKMODES_GET reply as the kernel
// sends it, with the OURS bitset in verbose form.
func encodeLinkModes(t *testing.T, speed uint32, duplex, autoneg uint8, noMask bool, bits []linkModeBit) []byte {
	t.Helper()
	ae := mdnetlink.NewAttributeEncoder()
	ae.Nested(unix.ETHTOOL_A_LINKMODES_HEADER, func(nae *mdnetlink.AttributeEncoder) error {
		nae.Uint32(unix.ETHTOOL_A_HEADER_DEV_INDEX, 4)
		nae.String(unix.ETHTOOL_A_HEADER_DEV_NAME, "enp1s0f1np1")
		return nil
	})
	ae.Uint8(unix.ETHTOOL_A_LINKMODES_AUTONEG, autoneg)
	ae.Nested(unix.ETHTOOL_A_LINKMODES_OURS, func(nae *mdnetlink.AttributeEncoder) error {
		nae.Flag(unix.ETHTOOL_A_BITSET_NOMASK, noMask)
		nae.Uint32(unix.ETHTOOL_A_BITSET_SIZE, 121)
		nae.Nested(unix.ETHTOOL_A_BITSET_BITS, func(bae *mdnetlink.AttributeEncoder) error {
			for _, b := range bits {
				bae.Nested(unix.ETHTOOL_A_BITSET_BITS_BIT, func(bit *mdnetlink.AttributeEncoder) error {
					bit.Uint32(unix.ETHTOOL_A_BITSET_BIT_INDEX, b.index)
					bit.String(unix.ETHTOOL_A_BITSET_BIT_NAME, b.name)
					bit.Flag(unix.ETHTOOL_A_BITSET_BIT_VALUE, b.set)
					return nil
				})
			}
			return nil
		})
		return nil
	})
	ae.Uint32(unix.ETHTOOL_A_LINKMODES_SPEED, speed)
	ae.Uint8(unix.ETHTOOL_A_LINKMODES_DUPLEX, duplex)
	ae.Uint32(unix.ETHTOOL_A_LINKMODES_LANES, 1)
	data, err := ae.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A 100G-capable port advertising up to 25G and linked at 25G: the mask
// is what it supports, the value what it advertises; non-speed modes
// (Autoneg, Pause, FEC) count for neither.
func TestParseLinkModes(t *testing.T) {
	data := encodeLinkModes(t, 25000, ethtoolDuplexFull, ethtoolAutonegEnable, false, []linkModeBit{
		{5, "1000baseT/Full", true},
		{6, "Autoneg", true},
		{10, "FIBRE", true},
		{13, "Pause", true},
		{31, "25000baseCR/Full", true},
		{52, "100000baseCR4/Full", false},
		{57, "50000baseCR2/Full", false},
		{50, "FEC_RS", true},
	})
	var link EthtoolLink
	if err := parseLinkModes(data, &link); err != nil {
		t.Fatalf("parseLinkModes: %v", err)
	}
	want := EthtoolLink{HasModes: true, SpeedMbps: 25000, Duplex: "full", Autoneg: "on", SupportedMaxMbps: 100000, AdvertisedMaxMbps: 25000}
	if link != want {
		t.Errorf("link = %+v, want %+v", link, want)
	}
}

// No link: SPEED_UNKNOWN and DUPLEX_UNKNOWN, autonegotiation off, nothing
// advertised.
func TestParseLinkModesUnknown(t *testing.T) {
	data := encodeLinkModes(t, 0xffffffff, 0xff, 0, false, []linkModeBit{
		{52, "100000baseCR4/Full", false},
	})
	var link EthtoolLink
	if err := parseLinkModes(data, &link); err != nil {
		t.Fatalf("parseLinkModes: %v", err)
	}
	want := EthtoolLink{HasModes: true, Duplex: "unknown", Autoneg: "off", SupportedMaxMbps: 100000}
	if link != want {
		t.Errorf("link = %+v, want %+v", link, want)
	}
}

// A bitset without a mask lists only its set bits, and no mask means no
// supported set.
func TestParseVerboseBitsetNoMask(t *testing.T) {
	data := encodeLinkModes(t, 10000, ethtoolDuplexHalf, ethtoolAutonegEnable, true, []linkModeBit{
		{12, "10000baseT/Full", false},
		{3, "1000baseT/Half", false},
	})
	var link EthtoolLink
	if err := parseLinkModes(data, &link); err != nil {
		t.Fatalf("parseLinkModes: %v", err)
	}
	if link.SupportedMaxMbps != 0 || link.AdvertisedMaxMbps != 10000 || link.Duplex != "half" {
		t.Errorf("link = %+v, want no supported max, 10000 advertised, half duplex", link)
	}
}

func TestParseLinkState(t *testing.T) {
	encode := func(link *uint8) []byte {
		ae := mdnetlink.NewAttributeEncoder()
		ae.Nested(unix.ETHTOOL_A_LINKSTATE_HEADER, func(nae *mdnetlink.AttributeEncoder) error {
			nae.String(unix.ETHTOOL_A_HEADER_DEV_NAME, "enp1s0f1np1")
			return nil
		})
		if link != nil {
			ae.Uint8(unix.ETHTOOL_A_LINKSTATE_LINK, *link)
		}
		ae.Uint32(unix.ETHTOOL_A_LINKSTATE_EXT_DOWN_CNT, 3)
		data, err := ae.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	up, down := uint8(1), uint8(0)
	for name, tc := range map[string]struct {
		attr *uint8
		want EthtoolLink
	}{
		"up":      {&up, EthtoolLink{HasLink: true, Link: true}},
		"down":    {&down, EthtoolLink{HasLink: true}},
		"unknown": {nil, EthtoolLink{}},
	} {
		var link EthtoolLink
		if err := parseLinkState(encode(tc.attr), &link); err != nil {
			t.Fatalf("%s: parseLinkState: %v", name, err)
		}
		if !reflect.DeepEqual(link, tc.want) {
			t.Errorf("%s: link = %+v, want %+v", name, link, tc.want)
		}
	}
}

func TestMaxLinkModeSpeed(t *testing.T) {
	for _, tc := range []struct {
		modes []string
		want  int
	}{
		{[]string{"10baseT/Half", "2500baseX/Full", "800000baseCR8/Full", "200000baseCR4/Full"}, 800000},
		{[]string{"Autoneg", "TP", "Backplane"}, 0},
		{nil, 0},
	} {
		if got := maxLinkModeSpeed(tc.modes); got != tc.want {
			t.Errorf("maxLinkModeSpeed(%v) = %d, want %d", tc.modes, got, tc.want)
		}
	}
}
