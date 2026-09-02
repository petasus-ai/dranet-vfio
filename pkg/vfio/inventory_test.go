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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink/nl"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
)

// inventoryFixture builds a node with one switchdev PF (netdev
// enp8s0f0np0) carrying two vfio-bound Ethernet VFs, one vfio-bound IB VF
// on another PF, one whole vfio-bound Ethernet PF, and one vfio-bound GPU
// that must never be advertised.
func inventoryFixture(t *testing.T) *sysfsBuilder {
	b := newSysfsBuilder(t)

	// Ethernet PF with netdev + 2 VFs bound to vfio-pci.
	b.device("0000:08:00.0", "0x020000", "mlx5_core", "")
	b.netdev("0000:08:00.0", "enp8s0f0np0", "p0")
	b.device("0000:08:00.2", "0x020000", "vfio-pci", "42")
	b.vf("0000:08:00.2", "0000:08:00.0", "0")
	b.device("0000:08:00.3", "0x020000", "vfio-pci", "43")
	b.vf("0000:08:00.3", "0000:08:00.0", "1")

	// InfiniBand PF with one vfio-bound VF.
	b.device("0000:0c:00.0", "0x020700", "mlx5_core", "")
	b.netdev("0000:0c:00.0", "ibp12s0", "")
	b.infiniband("0000:0c:00.0", "mlx5_1")
	b.device("0000:0c:00.2", "0x020700", "vfio-pci", "50")
	b.vf("0000:0c:00.2", "0000:0c:00.0", "0")

	// Whole Ethernet PF bound to vfio-pci (no netdev at all), Mellanox silicon.
	b.device("0000:51:00.0", "0x020000", "vfio-pci", "60")
	b.writeFile("0x15b3", "bus/pci/devices", "0000:51:00.0", "vendor")

	// GPU bound to vfio-pci: not a network class, never advertised.
	b.device("0000:65:00.0", "0x030200", "vfio-pci", "70")

	return b
}

func TestBuildDevices(t *testing.T) {
	b := inventoryFixture(t)
	inv := New("node-1", WithSysfs(b.ops()))

	devices, specs := inv.buildDevices()
	if len(devices) != 4 {
		t.Fatalf("expected 4 devices, got %d: %v", len(devices), deviceNames(devices))
	}

	byName := map[string]int{}
	for i, dev := range devices {
		byName[dev.Name] = i
	}

	// Ethernet VF.
	idx, ok := byName["pci-0000-08-00-2"]
	if !ok {
		t.Fatalf("VF device missing: %v", deviceNames(devices))
	}
	attrs := devices[idx].Attributes
	if got := *attrs[deviceattribute.StandardDeviceAttributePCIBusID].StringValue; got != "0000:08:00.2" {
		t.Fatalf("unexpected pciBusID: %q", got)
	}
	if got := *attrs[AttrDomain+"/kind"].StringValue; got != "vf" {
		t.Fatalf("unexpected kind: %q", got)
	}
	if got := *attrs[AttrDomain+"/linkType"].StringValue; got != "ethernet" {
		t.Fatalf("unexpected linkType: %q", got)
	}
	if got := *attrs[AttrDomain+"/pfName"].StringValue; got != "enp8s0f0np0" {
		t.Fatalf("unexpected pfName: %q", got)
	}
	if got := *attrs[AttrDomain+"/pfPciAddress"].StringValue; got != "0000:08:00.0" {
		t.Fatalf("unexpected pfPciAddress: %q", got)
	}
	if got := *attrs[AttrDomain+"/iommuGroup"].IntValue; got != 42 {
		t.Fatalf("unexpected iommuGroup: %d", got)
	}
	// No RDMA node on the PF and no Mellanox vendor id -> not RDMA-capable.
	if got := *attrs[AttrDomain+"/rdmaCapable"].BoolValue; got {
		t.Fatal("plain Ethernet VF must not be rdmaCapable")
	}
	spec := specs["pci-0000-08-00-2"]
	if spec.PFPCIAddress != "0000:08:00.0" || spec.VFIndex != 0 || spec.IOMMUGroup != 42 {
		t.Fatalf("unexpected spec: %#v", spec)
	}

	// IB VF is advertised with its link type.
	idx, ok = byName["pci-0000-0c-00-2"]
	if !ok {
		t.Fatalf("IB VF missing: %v", deviceNames(devices))
	}
	if got := *devices[idx].Attributes[AttrDomain+"/linkType"].StringValue; got != "infiniband" {
		t.Fatalf("unexpected IB linkType: %q", got)
	}
	// The vfio-bound VF inherits RDMA capability from its kernel-bound PF.
	if got := *devices[idx].Attributes[AttrDomain+"/rdmaCapable"].BoolValue; !got {
		t.Fatal("VF of an RDMA PF must be rdmaCapable")
	}

	// Whole vfio-bound PF.
	idx, ok = byName["pci-0000-51-00-0"]
	if !ok {
		t.Fatalf("PF device missing: %v", deviceNames(devices))
	}
	attrs = devices[idx].Attributes
	if got := *attrs[AttrDomain+"/kind"].StringValue; got != "pf" {
		t.Fatalf("unexpected kind: %q", got)
	}
	if _, hasVfIndex := attrs[AttrDomain+"/vfIndex"]; hasVfIndex {
		t.Fatal("PF must not carry a vfIndex attribute")
	}
	// vfio-bound whole PF exposes no RDMA node; the Mellanox vendor id stands in.
	if got := *attrs[AttrDomain+"/rdmaCapable"].BoolValue; !got {
		t.Fatal("Mellanox PF must be rdmaCapable")
	}

	// The GPU is not advertised.
	if _, ok := byName["pci-0000-65-00-0"]; ok {
		t.Fatal("non-network vfio device must not be advertised")
	}

	// GetVfioSpec resolves through the published state.
	inv.sync(true)
	if _, ok := inv.GetVfioSpec("pci-0000-08-00-3"); !ok {
		t.Fatal("GetVfioSpec failed for published device")
	}
	if _, err := inv.GetNetInterfaceName("pci-0000-08-00-3"); err == nil {
		t.Fatal("GetNetInterfaceName must fail for vfio devices")
	}
}

// TestBuildDevicesUplinkVlans: ethernet VFs publish their PF uplink's VLAN
// snapshot; InfiniBand and whole-PF devices publish none.
func TestBuildDevicesUplinkVlans(t *testing.T) {
	b := inventoryFixture(t)
	b.writeFile("1", "class/net/br0/bridge/vlan_filtering")

	nlops := newFakeNetlink()
	nlops.addLink("enp8s0f0np0", "device", 1, 2, 1500)
	nlops.addLink("bond0", "bond", 2, 3, 1500)
	nlops.addLink("br0", "bridge", 3, 0, 1500)
	nlops.vlanTable[2] = []*nl.BridgeVlanInfo{
		{Vid: 1, Flags: nl.BRIDGE_VLAN_INFO_PVID | nl.BRIDGE_VLAN_INFO_UNTAGGED},
		{Vid: 102}, {Vid: 100}, {Vid: 101}, {Vid: 200},
	}

	inv := New("node-1", WithSysfs(b.ops()), WithNetlink(nlops))
	devices, _ := inv.buildDevices()

	byName := map[string]resourceapi.Device{}
	for _, dev := range devices {
		byName[dev.Name] = dev
	}

	for _, name := range []string{"pci-0000-08-00-2", "pci-0000-08-00-3"} {
		attrs := byName[name].Attributes
		if got := *attrs[AttrDomain+"/uplink"].StringValue; got != "bond0" {
			t.Fatalf("%s: unexpected uplink %q", name, got)
		}
		if got := *attrs[AttrDomain+"/bridge"].StringValue; got != "br0" {
			t.Fatalf("%s: unexpected bridge %q", name, got)
		}
		if got := *attrs[AttrDomain+"/vlanFiltering"].BoolValue; !got {
			t.Fatalf("%s: expected vlanFiltering true", name)
		}
		if got := *attrs[AttrDomain+"/uplinkVlans"].StringValue; got != "1,100-102,200" {
			t.Fatalf("%s: unexpected uplinkVlans %q", name, got)
		}
		if got := *attrs[AttrDomain+"/untaggedVlan"].IntValue; got != 1 {
			t.Fatalf("%s: unexpected untaggedVlan %d", name, got)
		}
		if got := *attrs[AttrDomain+"/uplinkVlansExact"].StringValue; got != ",1,100,101,102,200," {
			t.Fatalf("%s: unexpected uplinkVlansExact %q", name, got)
		}
		if _, ok := attrs[AttrDomain+"/uplinkVlansTruncated"]; ok {
			t.Fatalf("%s: unexpected uplinkVlansTruncated", name)
		}
		if _, ok := attrs[AttrDomain+"/uplinkVlansExactOverflow"]; ok {
			t.Fatalf("%s: unexpected uplinkVlansExactOverflow", name)
		}
	}

	for _, name := range []string{"pci-0000-0c-00-2", "pci-0000-51-00-0"} {
		if _, ok := byName[name].Attributes[AttrDomain+"/uplink"]; ok {
			t.Fatalf("%s: must not carry uplink attributes", name)
		}
	}
}

// A PF that is its own uplink with no bridge master publishes the uplink
// name only — no bridge, filtering, or VLAN attributes.
func TestBuildDevicesUplinkNoBridge(t *testing.T) {
	b := inventoryFixture(t)

	nlops := newFakeNetlink()
	nlops.addLink("enp8s0f0np0", "device", 1, 0, 1500)

	inv := New("node-1", WithSysfs(b.ops()), WithNetlink(nlops))
	devices, _ := inv.buildDevices()

	for _, dev := range devices {
		if dev.Name != "pci-0000-08-00-2" {
			continue
		}
		if got := *dev.Attributes[AttrDomain+"/uplink"].StringValue; got != "enp8s0f0np0" {
			t.Fatalf("unexpected uplink %q", got)
		}
		for _, attr := range []string{"bridge", "vlanFiltering", "uplinkVlans", "untaggedVlan"} {
			if _, ok := dev.Attributes[resourceapi.QualifiedName(AttrDomain+"/"+attr)]; ok {
				t.Fatalf("unexpected attribute %s without a bridge", attr)
			}
		}
		return
	}
	t.Fatalf("VF device missing: %v", deviceNames(devices))
}

func deviceNames(devices []resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for _, dev := range devices {
		names = append(names, dev.Name)
	}
	return names
}

// TestBuildDevices_InfinibandGUIDs: an IB VF publishes the GUIDs its PF
// reports through rtnetlink; a whole IB PF (no host driver) and a VF whose PF
// reports zeros fall back to sriov-daemon's record; a function with neither
// source carries no GUID attributes at all.
func TestBuildDevices_InfinibandGUIDs(t *testing.T) {
	b := inventoryFixture(t)
	// Second IB VF on the same PF: unprogrammed on the PF, present in the record.
	b.device("0000:0c:00.3", "0x020700", "vfio-pci", "51")
	b.vf("0000:0c:00.3", "0000:0c:00.0", "1")
	// Whole IB PF bound to vfio-pci, recorded.
	b.device("0000:18:00.0", "0x020700", "vfio-pci", "80")
	// Whole IB PF bound to vfio-pci, not recorded anywhere.
	b.device("0000:3e:00.0", "0x020700", "vfio-pci", "81")

	recordPath := filepath.Join(t.TempDir(), "ib-guids.json")
	record := `{"version":1,"updatedAt":"2026-09-02T00:00:00Z","devices":{
	  "0000:18:00.0":{"kind":"pf","ibdev":"ibp24s0","nodeGUID":"3825f30300928354","portGUID":"3825f30300928354","recordedAt":"2026-09-02T00:00:00Z"},
	  "0000:0c:00.3":{"kind":"vf","pfPciAddress":"0000:0c:00.0","vfIndex":1,"nodeGUID":"3a25f30200928354","portGUID":"3a25f30200928354","recordedAt":"2026-09-02T00:00:00Z"}}}`
	if err := os.WriteFile(recordPath, []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	nlops := newFakeNetlink()
	var live VfIbGUID
	copy(live.NodeGUID[:], []byte{0x3a, 0x25, 0xf3, 0x01, 0x00, 0x92, 0x83, 0x54})
	live.PortGUID = live.NodeGUID
	nlops.vfIbGuids = map[string]map[int]VfIbGUID{"ibp12s0": {0: live, 1: {}}}

	inv := New("node-1", WithSysfs(b.ops()), WithNetlink(nlops), WithIbGuidFile(recordPath))
	devices, specs := inv.buildDevices()
	byName := map[string]resourceapi.Device{}
	for _, dev := range devices {
		byName[dev.Name] = dev
	}
	guid := func(name, attr string) string {
		if a, ok := byName[name].Attributes[resourceapi.QualifiedName(AttrDomain+"/"+attr)]; ok {
			return *a.StringValue
		}
		return "<absent>"
	}
	if got := guid("pci-0000-0c-00-2", "portGUID"); got != "3a25f30100928354" {
		t.Errorf("live VF portGUID = %s", got)
	}
	if got := guid("pci-0000-0c-00-3", "nodeGUID"); got != "3a25f30200928354" {
		t.Errorf("record-backed VF nodeGUID = %s", got)
	}
	if got := guid("pci-0000-18-00-0", "portGUID"); got != "3825f30300928354" {
		t.Errorf("whole PF portGUID = %s", got)
	}
	if got := guid("pci-0000-3e-00-0", "portGUID"); got != "<absent>" {
		t.Errorf("unrecorded PF must carry no portGUID, got %s", got)
	}
	if got := guid("pci-0000-08-00-2", "portGUID"); got != "<absent>" {
		t.Errorf("Ethernet VF must carry no portGUID, got %s", got)
	}
	if specs["pci-0000-18-00-0"].NodeGUID != "3825f30300928354" {
		t.Errorf("spec NodeGUID = %q", specs["pci-0000-18-00-0"].NodeGUID)
	}

	// A record written later is picked up on the next pass (mtime change).
	time.Sleep(10 * time.Millisecond)
	record2 := strings.Replace(record, `"0000:18:00.0":`, `"0000:3e:00.0":{"kind":"pf","nodeGUID":"3825f30300923f34","portGUID":"3825f30300923f34"},"0000:18:00.0":`, 1)
	if err := os.WriteFile(recordPath, []byte(record2), 0o644); err != nil {
		t.Fatal(err)
	}
	devices, _ = inv.buildDevices()
	for _, dev := range devices {
		byName[dev.Name] = dev
	}
	if got := guid("pci-0000-3e-00-0", "portGUID"); got != "3825f30300923f34" {
		t.Errorf("late record not picked up: %s", got)
	}
}
