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
	"testing"

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
	b.device("0000:0c:00.2", "0x020700", "vfio-pci", "50")
	b.vf("0000:0c:00.2", "0000:0c:00.0", "0")

	// Whole Ethernet PF bound to vfio-pci (no netdev at all).
	b.device("0000:51:00.0", "0x020000", "vfio-pci", "60")

	// GPU bound to vfio-pci: not a network class, never advertised.
	b.device("0000:65:00.0", "0x030200", "vfio-pci", "70")

	return b
}

const inventoryConfig = `
pools:
  - name: sriov-a
    pfNames: [enp8s0f0np0]
  - name: ib-compute
    linkType: infiniband
    pfNames: [ibp12s0]
  - name: dpdk-pf
    kind: pf
    pciAddresses: ["0000:51:00.*"]
`

func TestBuildDevices(t *testing.T) {
	b := inventoryFixture(t)
	inv := New(writeConfig(t, inventoryConfig), "node-1", WithSysfs(b.ops()))

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
	if got := *attrs[AttrResourceName].StringValue; got != "petasus.io/sriov-a" {
		t.Fatalf("unexpected resourceName: %q", got)
	}
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
	if got := *attrs[AttrDomain+"/iommuGroup"].IntValue; got != 42 {
		t.Fatalf("unexpected iommuGroup: %d", got)
	}
	spec := specs["pci-0000-08-00-2"]
	if spec.Pool != "sriov-a" || spec.PFPCIAddress != "0000:08:00.0" || spec.VFIndex != 0 || spec.IOMMUGroup != 42 {
		t.Fatalf("unexpected spec: %#v", spec)
	}

	// IB VF landed in its pool.
	idx, ok = byName["pci-0000-0c-00-2"]
	if !ok {
		t.Fatalf("IB VF missing: %v", deviceNames(devices))
	}
	if got := *devices[idx].Attributes[AttrResourceName].StringValue; got != "petasus.io/ib-compute" {
		t.Fatalf("unexpected IB resourceName: %q", got)
	}

	// Whole-PF pool with resourceName default.
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

func TestBuildDevicesUnmatchedAndBrokenConfig(t *testing.T) {
	b := inventoryFixture(t)

	// A config matching nothing advertises nothing.
	inv := New(writeConfig(t, "pools:\n  - {name: other, pfNames: [enp99s0]}\n"), "node-1", WithSysfs(b.ops()))
	devices, _ := inv.buildDevices()
	if len(devices) != 0 {
		t.Fatalf("expected no devices, got %v", deviceNames(devices))
	}

	// A broken config advertises nothing rather than everything.
	inv = New(writeConfig(t, "pools:\n  - {name: a}\n"), "node-1", WithSysfs(b.ops()))
	devices, _ = inv.buildDevices()
	if len(devices) != 0 {
		t.Fatalf("expected no devices for invalid config, got %v", deviceNames(devices))
	}
}

func TestBuildDevicesByPFPciAddress(t *testing.T) {
	b := inventoryFixture(t)

	config := "pools:\n  - name: by-pf\n    pfPciAddressesByNode:\n      node-1: [\"0000:08:00.0\"]\n"
	inv := New(writeConfig(t, config), "node-1", WithSysfs(b.ops()))
	devices, _ := inv.buildDevices()
	if len(devices) != 2 {
		t.Fatalf("expected the 2 VFs of 0000:08:00.0, got %v", deviceNames(devices))
	}
	for _, dev := range devices {
		if got := *dev.Attributes[AttrDomain+"/pool"].StringValue; got != "by-pf" {
			t.Fatalf("expected pool by-pf, got %q", got)
		}
	}

	// The same selector matches nothing on a node the map does not list.
	inv = New(writeConfig(t, config), "node-2", WithSysfs(b.ops()))
	devices, _ = inv.buildDevices()
	if len(devices) != 0 {
		t.Fatalf("expected no devices on an unlisted node, got %v", deviceNames(devices))
	}
}

func deviceNames(devices []resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for _, dev := range devices {
		names = append(names, dev.Name)
	}
	return names
}
