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
	"testing"
)

// sysfsBuilder assembles a fake /sys tree for tests.
type sysfsBuilder struct {
	t    *testing.T
	base string
}

func newSysfsBuilder(t *testing.T) *sysfsBuilder {
	return &sysfsBuilder{t: t, base: t.TempDir()}
}

func (b *sysfsBuilder) ops() SysfsOps { return NewSysfsWithBase(b.base) }

func (b *sysfsBuilder) mkdir(parts ...string) string {
	p := filepath.Join(append([]string{b.base}, parts...)...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		b.t.Fatal(err)
	}
	return p
}

func (b *sysfsBuilder) writeFile(content string, parts ...string) {
	p := filepath.Join(append([]string{b.base}, parts...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *sysfsBuilder) symlink(target string, parts ...string) {
	p := filepath.Join(append([]string{b.base}, parts...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		b.t.Fatal(err)
	}
}

// device creates a PCI device dir with a class file, optionally bound to a
// driver and belonging to an IOMMU group.
func (b *sysfsBuilder) device(bdf, class, driver string, iommuGroup string) {
	b.mkdir("bus/pci/devices", bdf)
	b.writeFile(class, "bus/pci/devices", bdf, "class")
	if driver != "" {
		b.symlink("../../../bus/pci/drivers/"+driver, "bus/pci/devices", bdf, "driver")
		b.symlink("../../devices/"+bdf, "bus/pci/drivers", driver, bdf)
	}
	if iommuGroup != "" {
		b.symlink("../../../kernel/iommu_groups/"+iommuGroup, "bus/pci/devices", bdf, "iommu_group")
		b.symlink("../../../../bus/pci/devices/"+bdf, "kernel/iommu_groups", iommuGroup, "devices", bdf)
	}
}

func (b *sysfsBuilder) vf(vfBdf, pfBdf string, index string) {
	b.symlink("../"+pfBdf, "bus/pci/devices", vfBdf, "physfn")
	b.symlink("../"+vfBdf, "bus/pci/devices", pfBdf, "virtfn"+index)
}

// infiniband gives a device an RDMA device node, as the kernel driver of an
// RDMA-capable function does.
func (b *sysfsBuilder) infiniband(bdf, ibdev string) {
	b.mkdir("bus/pci/devices", bdf, "infiniband", ibdev)
}

func (b *sysfsBuilder) netdev(pfBdf, name, physPortName string) {
	b.mkdir("bus/pci/devices", pfBdf, "net", name)
	if physPortName != "" {
		b.writeFile(physPortName, "bus/pci/devices", pfBdf, "net", name, "phys_port_name")
	}
	b.writeFile(physPortName, "class/net", name, "phys_port_name")
}

func TestListVFIODevices(t *testing.T) {
	b := newSysfsBuilder(t)
	b.device("0000:08:00.2", "0x020000", "vfio-pci", "42")
	b.device("0000:08:00.3", "0x020000", "vfio-pci", "43")
	b.mkdir("bus/pci/drivers/vfio-pci/module") // non-BDF entries are ignored
	b.writeFile("", "bus/pci/drivers/vfio-pci/bind")

	devices, err := b.ops().ListVFIODevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %v", devices)
	}

	// Missing driver dir: empty, no error.
	empty := newSysfsBuilder(t)
	devices, err = empty.ops().ListVFIODevices()
	if err != nil || len(devices) != 0 {
		t.Fatalf("expected no devices and no error, got %v, %v", devices, err)
	}
}

func TestIsVfAndPfResolution(t *testing.T) {
	b := newSysfsBuilder(t)
	b.device("0000:08:00.0", "0x020000", "mlx5_core", "")
	b.device("0000:08:00.2", "0x020000", "vfio-pci", "42")
	b.vf("0000:08:00.2", "0000:08:00.0", "0")
	ops := b.ops()

	isVf, err := ops.IsVf("0000:08:00.2")
	if err != nil || !isVf {
		t.Fatalf("expected VF, got %v, %v", isVf, err)
	}
	isVf, err = ops.IsVf("0000:08:00.0")
	if err != nil || isVf {
		t.Fatalf("expected PF, got %v, %v", isVf, err)
	}
	if _, err := ops.IsVf("0000:ff:00.0"); err == nil {
		t.Fatal("expected error for missing device")
	}

	pfPci, err := ops.GetPfPci("0000:08:00.2")
	if err != nil || pfPci != "0000:08:00.0" {
		t.Fatalf("expected PF 0000:08:00.0, got %q, %v", pfPci, err)
	}
	idx, err := ops.GetVfIndex("0000:08:00.0", "0000:08:00.2")
	if err != nil || idx != 0 {
		t.Fatalf("expected VF index 0, got %d, %v", idx, err)
	}
	driver, err := ops.GetDriver("0000:08:00.2")
	if err != nil || driver != "vfio-pci" {
		t.Fatalf("expected vfio-pci, got %q, %v", driver, err)
	}
	group, err := ops.GetIOMMUGroup("0000:08:00.2")
	if err != nil || group != 42 {
		t.Fatalf("expected group 42, got %d, %v", group, err)
	}
	members, err := ops.ListIOMMUGroupDevices(42)
	if err != nil || len(members) != 1 || members[0] != "0000:08:00.2" {
		t.Fatalf("expected [0000:08:00.2], got %v, %v", members, err)
	}
}

func TestGetPfName(t *testing.T) {
	tests := []struct {
		name    string
		netdevs map[string]string // name -> phys_port_name
		want    string
		wantErr bool
	}{
		{
			name:    "switchdev uplink among representors",
			netdevs: map[string]string{"enp8s0f0np0": "p0", "enp8s0f0r0": "pf0vf0", "enp8s0f0r1": "pf0vf1"},
			want:    "enp8s0f0np0",
		},
		{
			name:    "legacy PF without phys_port_name",
			netdevs: map[string]string{"enp8s0f0": ""},
			want:    "enp8s0f0",
		},
		{
			name:    "SF and host-PF representors are not the uplink",
			netdevs: map[string]string{"enp8s0f0sf": "pf0sf0", "enp8s0f0hpf": "pf0hpf"},
			wantErr: true,
		},
		{
			name:    "no netdev",
			netdevs: map[string]string{},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newSysfsBuilder(t)
			b.device("0000:08:00.0", "0x020000", "", "")
			b.mkdir("bus/pci/devices/0000:08:00.0/net")
			for name, port := range tc.netdevs {
				b.netdev("0000:08:00.0", name, port)
			}
			got, err := b.ops().GetPfName("0000:08:00.0")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("expected %q, got %q, %v", tc.want, got, err)
			}
		})
	}
}

func TestGetRepresentor(t *testing.T) {
	b := newSysfsBuilder(t)
	b.device("0000:08:00.0", "0x020000", "", "")
	b.netdev("0000:08:00.0", "enp8s0f0np0", "p0")
	b.netdev("0000:08:00.0", "rep0", "pf0vf0")
	b.netdev("0000:08:00.0", "rep3", "pf0vf3")
	b.netdev("0000:08:00.0", "hostrep3", "c1pf0vf3") // external controller
	ops := b.ops()

	rep, err := ops.GetRepresentor("0000:08:00.0", PortID{PF: 0}, 3)
	if err != nil || rep != "rep3" {
		t.Fatalf("expected rep3, got %q, %v", rep, err)
	}
	rep, err = ops.GetRepresentor("0000:08:00.0", PortID{Controller: "1", PF: 0}, 3)
	if err != nil || rep != "hostrep3" {
		t.Fatalf("expected hostrep3, got %q, %v", rep, err)
	}
	if _, err := ops.GetRepresentor("0000:08:00.0", PortID{PF: 0}, 7); err == nil {
		t.Fatal("expected error for missing representor")
	}
}

func TestGetUplinkPort(t *testing.T) {
	tests := []struct {
		portName string
		want     PortID
		wantErr  bool
	}{
		{portName: "p0", want: PortID{PF: 0}},
		{portName: "p1", want: PortID{PF: 1}},
		{portName: "l0p0s0", want: PortID{PF: 0}},
		{portName: "c2p1", want: PortID{Controller: "2", PF: 1}},
		{portName: "pf0", want: PortID{PF: 0}},
		{portName: "pf0vf3", wantErr: true},
		{portName: "", wantErr: true},
		{portName: "garbage!", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.portName, func(t *testing.T) {
			b := newSysfsBuilder(t)
			b.writeFile(tc.portName, "class/net/uplink0/phys_port_name")
			got, err := b.ops().GetUplinkPort("uplink0")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("expected %v, got %v, %v", tc.want, got, err)
			}
		})
	}
}

func TestBridgeAttributes(t *testing.T) {
	b := newSysfsBuilder(t)
	b.writeFile("1", "class/net/br0/bridge/vlan_filtering")
	b.writeFile("1", "class/net/br0/bridge/default_pvid")
	b.writeFile("0", "class/net/br1/bridge/vlan_filtering")
	ops := b.ops()

	filtering, err := ops.GetBridgeVlanFiltering("br0")
	if err != nil || !filtering {
		t.Fatalf("expected vlan_filtering on, got %v, %v", filtering, err)
	}
	filtering, err = ops.GetBridgeVlanFiltering("br1")
	if err != nil || filtering {
		t.Fatalf("expected vlan_filtering off, got %v, %v", filtering, err)
	}
	if _, err := ops.GetBridgeVlanFiltering("missing"); err == nil {
		t.Fatal("expected error for missing bridge")
	}
	pvid, err := ops.GetBridgeDefaultPvid("br0")
	if err != nil || pvid != 1 {
		t.Fatalf("expected default_pvid 1, got %d, %v", pvid, err)
	}
}

func TestGetNumaNode(t *testing.T) {
	b := newSysfsBuilder(t)
	b.device("0000:08:00.0", "0x020000", "", "")
	b.writeFile("1", "bus/pci/devices/0000:08:00.0/numa_node")
	b.device("0000:09:00.0", "0x020000", "", "")

	if numa, err := b.ops().GetNumaNode("0000:08:00.0"); err != nil || numa != 1 {
		t.Fatalf("expected numa 1, got %d, %v", numa, err)
	}
	if numa, err := b.ops().GetNumaNode("0000:09:00.0"); err != nil || numa != -1 {
		t.Fatalf("expected numa -1 for missing file, got %d, %v", numa, err)
	}
}
