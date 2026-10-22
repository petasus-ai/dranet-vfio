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
	"maps"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"k8s.io/utils/ptr"
)

// fakeSysfs implements SysfsOps from fixed answers.
type fakeSysfs struct {
	SysfsOps // panic on anything not overridden

	isVf          map[string]bool
	class         map[string]string
	driver        map[string]string
	pfPci         map[string]string
	pfName        map[string]string
	vfIndex       map[string]int
	uplinkPort    map[string]PortID
	representor   string
	vlanFiltering map[string]bool
	groupDevices  map[int][]string
}

func (f *fakeSysfs) IsVf(dev string) (bool, error) {
	v, ok := f.isVf[dev]
	if !ok {
		return false, fmt.Errorf("PCI device %s not found in sysfs", dev)
	}
	return v, nil
}
func (f *fakeSysfs) GetPciClass(dev string) (string, error) { return f.class[dev], nil }
func (f *fakeSysfs) GetDriver(dev string) (string, error) {
	d, ok := f.driver[dev]
	if !ok {
		return "", fmt.Errorf("no driver for %s", dev)
	}
	return d, nil
}
func (f *fakeSysfs) GetPfPci(dev string) (string, error)   { return f.pfPci[dev], nil }
func (f *fakeSysfs) GetPfName(pf string) (string, error)   { return f.pfName[pf], nil }
func (f *fakeSysfs) GetVfIndex(pf, vf string) (int, error) { return f.vfIndex[vf], nil }
func (f *fakeSysfs) GetUplinkPort(pfName string) (PortID, error) {
	port, ok := f.uplinkPort[pfName]
	if !ok {
		return PortID{}, fmt.Errorf("empty phys_port_name for %s", pfName)
	}
	return port, nil
}
func (f *fakeSysfs) GetRepresentor(pfPci string, port PortID, vfIndex int) (string, error) {
	if f.representor == "" {
		return "", fmt.Errorf("representor not found")
	}
	return f.representor, nil
}
func (f *fakeSysfs) GetBridgeVlanFiltering(bridge string) (bool, error) {
	return f.vlanFiltering[bridge], nil
}
func (f *fakeSysfs) ListIOMMUGroupDevices(group int) ([]string, error) {
	return f.groupDevices[group], nil
}

// fakeNetlink implements NetlinkOps, records mutations and keeps a VF-info
// table that LinkSetVf* updates so MAC read-back verification passes.
type fakeNetlink struct {
	links       map[string]netlink.Link
	byIdx       map[int]netlink.Link
	vfInfo      map[string][]*VfInfo // by PF name
	eswitchMode map[string]string
	vlanTable   map[int32][]*nl.BridgeVlanInfo
	vfIbGuids   map[string]map[int]VfIbGUID // by PF name
	calls       []string
	failOn      map[string]error

	// joinPvid is the default_pvid a port gets on joining a bridge.
	joinPvid uint16
	// dumpMisses is how many next BridgeVlanList dumps leave a port out.
	dumpMisses map[int32]int
}

func newFakeNetlink() *fakeNetlink {
	return &fakeNetlink{
		links:       map[string]netlink.Link{},
		byIdx:       map[int]netlink.Link{},
		vfInfo:      map[string][]*VfInfo{},
		eswitchMode: map[string]string{},
		vlanTable:   map[int32][]*nl.BridgeVlanInfo{},
		failOn:      map[string]error{},
	}
}

func (f *fakeNetlink) addLink(name, linkType string, index, master, mtu int) {
	link := &netlink.GenericLink{
		LinkAttrs: netlink.LinkAttrs{Name: name, Index: index, MasterIndex: master, MTU: mtu},
		LinkType:  linkType,
	}
	f.links[name] = link
	f.byIdx[index] = link
}

func (f *fakeNetlink) record(call string) error {
	f.calls = append(f.calls, call)
	return f.failOn[call]
}

func (f *fakeNetlink) LinkGetVfIbGUIDs(pfName string) (map[int]VfIbGUID, error) {
	if f.vfIbGuids == nil {
		return nil, fmt.Errorf("no VF GUID info for %s", pfName)
	}
	return f.vfIbGuids[pfName], nil
}

func (f *fakeNetlink) LinkByName(name string) (netlink.Link, error) {
	link, ok := f.links[name]
	if !ok {
		return nil, netlink.LinkNotFoundError{}
	}
	return link, nil
}
func (f *fakeNetlink) LinkByIndex(index int) (netlink.Link, error) {
	link, ok := f.byIdx[index]
	if !ok {
		return nil, netlink.LinkNotFoundError{}
	}
	return link, nil
}
func (f *fakeNetlink) LinkSetUp(link netlink.Link) error {
	return f.record("up " + link.Attrs().Name)
}
func (f *fakeNetlink) LinkSetDown(link netlink.Link) error {
	return f.record("down " + link.Attrs().Name)
}

// LinkSetMaster gives a port joining a bridge the default PVID
// (br_vlan.c:nbp_vlan_init); re-enslaving it to its bridge keeps its VLANs.
func (f *fakeNetlink) LinkSetMaster(link, master netlink.Link) error {
	if err := f.record(fmt.Sprintf("master %s %s", link.Attrs().Name, master.Attrs().Name)); err != nil {
		return err
	}
	if link.Attrs().MasterIndex == master.Attrs().Index {
		return nil
	}
	link.Attrs().MasterIndex = master.Attrs().Index
	delete(f.vlanTable, portIndex(link))
	if f.joinPvid != 0 {
		f.vlanTable[portIndex(link)] = []*nl.BridgeVlanInfo{{Vid: f.joinPvid, Flags: nl.BRIDGE_VLAN_INFO_PVID | nl.BRIDGE_VLAN_INFO_UNTAGGED}}
	}
	return nil
}

// LinkSetNoMaster flushes the port's VLANs (br_vlan.c:nbp_vlan_flush).
func (f *fakeNetlink) LinkSetNoMaster(link netlink.Link) error {
	if err := f.record("nomaster " + link.Attrs().Name); err != nil {
		return err
	}
	link.Attrs().MasterIndex = 0
	delete(f.vlanTable, portIndex(link))
	return nil
}

func portIndex(link netlink.Link) int32 {
	return int32(link.Attrs().Index) //nolint:gosec // ifindex is int32 in the kernel ABI
}
func (f *fakeNetlink) LinkSetMTU(link netlink.Link, mtu int) error {
	if err := f.record(fmt.Sprintf("mtu %s %d", link.Attrs().Name, mtu)); err != nil {
		return err
	}
	link.Attrs().MTU = mtu
	return nil
}
func (f *fakeNetlink) LinkSetVfHardwareAddr(link netlink.Link, vf int, addr net.HardwareAddr) error {
	if err := f.record(fmt.Sprintf("vf-mac %s %d %s", link.Attrs().Name, vf, addr)); err != nil {
		return err
	}
	f.vfInfo[link.Attrs().Name][vf].Mac = addr
	return nil
}
func (f *fakeNetlink) LinkSetVfVlan(link netlink.Link, vf, vlan int) error {
	return f.record(fmt.Sprintf("vf-vlan %s %d %d", link.Attrs().Name, vf, vlan))
}
func (f *fakeNetlink) LinkSetVfState(link netlink.Link, vf int, state uint32) error {
	return f.record(fmt.Sprintf("vf-state %s %d %d", link.Attrs().Name, vf, state))
}

// BridgeVlanAdd adds or updates the port's VLAN; a new PVID replaces the old
// one (br_vlan.c:__vlan_add_pvid).
func (f *fakeNetlink) BridgeVlanAdd(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	if err := f.record(fmt.Sprintf("vlan-add %s %d pvid=%v untagged=%v", link.Attrs().Name, vid, pvid, untagged)); err != nil {
		return err
	}
	var flags uint16
	if pvid {
		flags |= nl.BRIDGE_VLAN_INFO_PVID
	}
	if untagged {
		flags |= nl.BRIDGE_VLAN_INFO_UNTAGGED
	}
	var vlans []*nl.BridgeVlanInfo
	for _, vi := range f.vlanTable[portIndex(link)] {
		if vi.Vid == vid {
			continue
		}
		if pvid {
			vi = &nl.BridgeVlanInfo{Vid: vi.Vid, Flags: vi.Flags &^ nl.BRIDGE_VLAN_INFO_PVID}
		}
		vlans = append(vlans, vi)
	}
	f.vlanTable[portIndex(link)] = append(vlans, &nl.BridgeVlanInfo{Vid: vid, Flags: flags})
	return nil
}
func (f *fakeNetlink) BridgeVlanDel(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	if err := f.record(fmt.Sprintf("vlan-del %s %d", link.Attrs().Name, vid)); err != nil {
		return err
	}
	var vlans []*nl.BridgeVlanInfo
	for _, vi := range f.vlanTable[portIndex(link)] {
		if vi.Vid != vid {
			vlans = append(vlans, vi)
		}
	}
	f.vlanTable[portIndex(link)] = vlans
	return nil
}

// portVlans renders a port's VLANs as "vid[:pvid][:untagged]", ascending.
func (f *fakeNetlink) portVlans(index int32) []string {
	var out []string
	for _, vi := range f.vlanTable[index] {
		s := strconv.Itoa(int(vi.Vid))
		if vi.PortVID() {
			s += ":pvid"
		}
		if vi.EngressUntag() {
			s += ":untagged"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
func (f *fakeNetlink) BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error) {
	dump := maps.Clone(f.vlanTable)
	for idx, n := range f.dumpMisses {
		if n > 0 {
			delete(dump, idx)
			f.dumpMisses[idx] = n - 1
		}
	}
	return dump, nil
}
func (f *fakeNetlink) DevLinkGetEswitchMode(pfPci string) (string, error) {
	mode, ok := f.eswitchMode[pfPci]
	if !ok {
		return "", fmt.Errorf("devlink get device %s: not found", pfPci)
	}
	return mode, nil
}
func (f *fakeNetlink) LinkGetVfInfo(pfName string, vf int) (*VfInfo, error) {
	vfs, ok := f.vfInfo[pfName]
	if !ok || vf >= len(vfs) {
		return nil, fmt.Errorf("PF %s reports no VF info", pfName)
	}
	info := *vfs[vf]
	return &info, nil
}

func (f *fakeNetlink) hasCall(t *testing.T, want string) {
	t.Helper()
	for _, call := range f.calls {
		if call == want {
			return
		}
	}
	t.Fatalf("expected call %q, got %v", want, f.calls)
}

const (
	testVF = "0000:08:00.2"
	testPF = "0000:08:00.0"
)

func legacyFixture() (*fakeSysfs, *fakeNetlink, *Configurer) {
	sysfs := &fakeSysfs{
		isVf:    map[string]bool{testVF: true},
		class:   map[string]string{testVF: "0x020000"},
		driver:  map[string]string{testVF: "vfio-pci"},
		pfPci:   map[string]string{testVF: testPF},
		pfName:  map[string]string{testPF: "enp8s0f0"},
		vfIndex: map[string]int{testVF: 3},
	}
	nlops := newFakeNetlink()
	nlops.addLink("enp8s0f0", "device", 2, 0, 9000)
	nlops.eswitchMode[testPF] = "legacy"
	origMac, _ := net.ParseMAC("aa:bb:cc:dd:ee:03")
	vfs := make([]*VfInfo, 4)
	for i := range vfs {
		vfs[i] = &VfInfo{}
	}
	vfs[3] = &VfInfo{Mac: origMac, Vlan: 7, LinkState: 2}
	nlops.vfInfo["enp8s0f0"] = vfs

	configurer := NewConfigurer(sysfs, nlops, "/tmp")
	configurer.acquireLock = func(dir, bridge string) (*Lock, error) { return &Lock{}, nil }
	return sysfs, nlops, configurer
}

func TestApplyLegacy(t *testing.T) {
	_, nlops, configurer := legacyFixture()
	params := &ClaimParams{MAC: "02:00:00:aa:bb:cc", Vlan: ptr.To(100)}

	state, err := planAndApply(configurer, Spec{PCIAddress: testVF}, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != ModeLegacy || state.PfName != "enp8s0f0" || state.VfIndex != 3 || state.Vlan != 100 {
		t.Fatalf("unexpected state: %#v", state)
	}
	// Originals captured before mutation.
	if state.OrigMAC != "aa:bb:cc:dd:ee:03" || state.OrigVlan != 7 || state.OrigLinkState != 2 {
		t.Fatalf("unexpected originals: %#v", state)
	}
	nlops.hasCall(t, "vf-mac enp8s0f0 3 02:00:00:aa:bb:cc")
	nlops.hasCall(t, "vf-vlan enp8s0f0 3 100")
	nlops.hasCall(t, "vf-state enp8s0f0 3 0")

	// Restore puts the originals back.
	nlops.calls = nil
	if errs := configurer.Restore(state); len(errs) > 0 {
		t.Fatalf("restore errors: %v", errs)
	}
	nlops.hasCall(t, "vf-mac enp8s0f0 3 aa:bb:cc:dd:ee:03")
	nlops.hasCall(t, "vf-vlan enp8s0f0 3 7")
	nlops.hasCall(t, "vf-state enp8s0f0 3 2")
}

func TestApplyLegacyRollbackOnFailure(t *testing.T) {
	_, nlops, configurer := legacyFixture()
	nlops.failOn["vf-vlan enp8s0f0 3 100"] = fmt.Errorf("EINVAL")
	params := &ClaimParams{MAC: "02:00:00:aa:bb:cc", Vlan: ptr.To(100)}

	_, err := planAndApply(configurer, Spec{PCIAddress: testVF}, params, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	// The MAC set before the failure was rolled back to the original.
	nlops.hasCall(t, "vf-mac enp8s0f0 3 aa:bb:cc:dd:ee:03")
}

func TestApplyPriorStateReused(t *testing.T) {
	_, nlops, configurer := legacyFixture()
	prior := &HostState{
		Mode: ModeLegacy, VfPci: testVF, PfName: "enp8s0f0", VfIndex: 3,
		OrigMAC: "00:11:22:33:44:55", OrigVlan: 9,
	}
	state, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{MAC: "02:00:00:aa:bb:cc"}, prior)
	if err != nil {
		t.Fatal(err)
	}
	// The retry keeps the earlier originals instead of re-reading the
	// kernel (which now reports our applied values).
	if state.OrigMAC != "00:11:22:33:44:55" || state.OrigVlan != 9 {
		t.Fatalf("prior originals not reused: %#v", state)
	}
	_ = nlops
}

func TestApplyRejectsWrongDriverAndClass(t *testing.T) {
	sysfs, _, configurer := legacyFixture()

	sysfs.driver[testVF] = "mlx5_core"
	if _, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{}, nil); err == nil ||
		!strings.Contains(err.Error(), "expected vfio-pci") {
		t.Fatalf("expected driver error, got %v", err)
	}

	sysfs.driver[testVF] = "vfio-pci"
	sysfs.class[testVF] = "0x030000" // GPU
	if _, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{}, nil); err == nil ||
		!strings.Contains(err.Error(), "expected a network controller") {
		t.Fatalf("expected class error, got %v", err)
	}
}

func TestApplyPassthrough(t *testing.T) {
	sysfs := &fakeSysfs{
		isVf:   map[string]bool{"0000:0c:00.2": true, "0000:51:00.0": false},
		class:  map[string]string{"0000:0c:00.2": "0x020700", "0000:51:00.0": "0x020000"},
		driver: map[string]string{"0000:0c:00.2": "vfio-pci", "0000:51:00.0": "vfio-pci"},
		pfPci:  map[string]string{"0000:0c:00.2": "0000:0c:00.0"},
	}
	configurer := NewConfigurer(sysfs, newFakeNetlink(), "/tmp")

	// InfiniBand VF: passthrough with the PF recorded.
	state, err := planAndApply(configurer, Spec{PCIAddress: "0000:0c:00.2"}, &ClaimParams{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != ModePassthrough || state.PfPci != "0000:0c:00.0" {
		t.Fatalf("unexpected state: %#v", state)
	}
	if errs := configurer.Restore(state); len(errs) > 0 {
		t.Fatalf("passthrough restore must be a no-op, got %v", errs)
	}

	// Ethernet PF: passthrough as well.
	state, err = planAndApply(configurer, Spec{PCIAddress: "0000:51:00.0"}, &ClaimParams{}, nil)
	if err != nil || state.Mode != ModePassthrough {
		t.Fatalf("unexpected result: %#v, %v", state, err)
	}

	// Any VF option on a passthrough device fails prepare.
	_, err = planAndApply(configurer, Spec{PCIAddress: "0000:0c:00.2"}, &ClaimParams{MAC: "02:00:00:aa:bb:cc"}, nil)
	if err == nil || !strings.Contains(err.Error(), "handed through without host-side configuration") {
		t.Fatalf("expected passthrough option rejection, got %v", err)
	}
}

func switchdevFixture() (*fakeSysfs, *fakeNetlink, *Configurer) {
	sysfs := &fakeSysfs{
		isVf:          map[string]bool{testVF: true},
		class:         map[string]string{testVF: "0x020000"},
		driver:        map[string]string{testVF: "vfio-pci"},
		pfPci:         map[string]string{testVF: testPF},
		pfName:        map[string]string{testPF: "enp8s0f0np0"},
		vfIndex:       map[string]int{testVF: 3},
		uplinkPort:    map[string]PortID{"enp8s0f0np0": {PF: 0}},
		representor:   "enp8s0f0r3",
		vlanFiltering: map[string]bool{"br-ex": true},
	}
	nlops := newFakeNetlink()
	nlops.eswitchMode[testPF] = "switchdev"
	nlops.joinPvid = 1
	nlops.addLink("br-ex", "bridge", 10, 0, 9000)
	nlops.addLink("enp8s0f0np0", "device", 2, 10, 9000) // uplink enslaved to br-ex
	nlops.addLink("enp8s0f0r3", "device", 20, 0, 1500)  // representor, MTU differs
	origMac, _ := net.ParseMAC("aa:bb:cc:dd:ee:03")
	vfs := make([]*VfInfo, 4)
	for i := range vfs {
		vfs[i] = &VfInfo{}
	}
	vfs[3] = &VfInfo{Mac: origMac}
	nlops.vfInfo["enp8s0f0np0"] = vfs
	// Uplink carries VLAN 100 (tagged) and 1 untagged.
	nlops.vlanTable[2] = []*nl.BridgeVlanInfo{
		{Vid: 1, Flags: nl.BRIDGE_VLAN_INFO_PVID | nl.BRIDGE_VLAN_INFO_UNTAGGED},
		{Vid: 100},
	}

	configurer := NewConfigurer(sysfs, nlops, "/tmp")
	configurer.acquireLock = func(dir, bridge string) (*Lock, error) { return &Lock{}, nil }
	return sysfs, nlops, configurer
}

func TestApplySwitchdev(t *testing.T) {
	_, nlops, configurer := switchdevFixture()
	params := &ClaimParams{MAC: "02:00:00:aa:bb:cc", Vlan: ptr.To(100)}

	state, err := planAndApply(configurer, Spec{PCIAddress: testVF}, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != ModeSwitchdev || state.Representor != "enp8s0f0r3" ||
		state.Bridge != "br-ex" || state.Uplink != "enp8s0f0np0" ||
		state.Vlan != 100 || !state.VlanFiltering || state.OrigRepMTU != 1500 {
		t.Fatalf("unexpected state: %#v", state)
	}
	nlops.hasCall(t, "mtu enp8s0f0r3 9000")
	nlops.hasCall(t, "master enp8s0f0r3 br-ex")
	nlops.hasCall(t, "vf-mac enp8s0f0np0 3 02:00:00:aa:bb:cc")
	nlops.hasCall(t, "vlan-add enp8s0f0r3 100 pvid=true untagged=true")
	nlops.hasCall(t, "vlan-del enp8s0f0r3 1") // default PVID removed
	nlops.hasCall(t, "up enp8s0f0r3")
	if got, want := nlops.portVlans(20), []string{"100:pvid:untagged"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("representor VLANs = %v, want %v", got, want)
	}

	// Restore detaches and restores VF settings, VLAN and MTU.
	nlops.calls = nil
	if errs := configurer.Restore(state); len(errs) > 0 {
		t.Fatalf("restore errors: %v", errs)
	}
	nlops.hasCall(t, "vf-mac enp8s0f0np0 3 aa:bb:cc:dd:ee:03")
	nlops.hasCall(t, "vlan-del enp8s0f0r3 100")
	nlops.hasCall(t, "nomaster enp8s0f0r3")
	nlops.hasCall(t, "down enp8s0f0r3")
	nlops.hasCall(t, "mtu enp8s0f0r3 1500")
}

// A VF reallocated before its previous claim was unprepared keeps its
// representor on the bridge with that claim's VLANs; prepare leaves the port
// with the new claim's VLAN only, and fails when another one cannot go.
func TestApplySwitchdevReassignedRepresentor(t *testing.T) {
	reassigned := func() (*fakeNetlink, *Configurer) {
		_, nlops, configurer := switchdevFixture()
		nlops.vlanTable[2] = append(nlops.vlanTable[2], &nl.BridgeVlanInfo{Vid: 200})
		nlops.addLink("enp8s0f0r3", "device", 20, 10, 9000) // still enslaved to br-ex
		nlops.vlanTable[20] = []*nl.BridgeVlanInfo{
			{Vid: 100, Flags: nl.BRIDGE_VLAN_INFO_PVID | nl.BRIDGE_VLAN_INFO_UNTAGGED},
			{Vid: 1},
			{Vid: 300, Flags: nl.BRIDGE_VLAN_INFO_UNTAGGED},
		}
		return nlops, configurer
	}

	nlops, configurer := reassigned()
	if _, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{Vlan: ptr.To(200)}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := nlops.portVlans(20), []string{"200:pvid:untagged"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("representor VLANs = %v, want %v", got, want)
	}

	// A dump that misses the port is retried.
	nlops, configurer = reassigned()
	nlops.dumpMisses = map[int32]int{20: bridgeVlanDumpAttempts - 1}
	if _, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{Vlan: ptr.To(200)}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := nlops.portVlans(20), []string{"200:pvid:untagged"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("representor VLANs after a missed dump = %v, want %v", got, want)
	}

	// Prepare fails when no dump lists the port.
	nlops, configurer = reassigned()
	nlops.dumpMisses = map[int32]int{20: bridgeVlanDumpAttempts + 1}
	if _, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{Vlan: ptr.To(200)}, nil); err == nil {
		t.Fatal("prepare succeeded although no dump listed the representor")
	}
	nlops.hasCall(t, "nomaster enp8s0f0r3")

	nlops, configurer = reassigned()
	nlops.failOn["vlan-del enp8s0f0r3 100"] = fmt.Errorf("EBUSY")
	_, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{Vlan: ptr.To(200)}, nil)
	if err == nil || !strings.Contains(err.Error(), "VLAN 100") {
		t.Fatalf("expected a failed prepare naming VLAN 100, got %v", err)
	}
	nlops.hasCall(t, "nomaster enp8s0f0r3")
	for _, call := range nlops.calls {
		if call == "up enp8s0f0r3" {
			t.Fatalf("representor brought up with VLAN 100 still on it: %v", nlops.calls)
		}
	}
}

// Plan only reads: the driver stores its result before the host changes.
func TestPlanChangesNothing(t *testing.T) {
	for name, fixture := range map[string]func() (*fakeSysfs, *fakeNetlink, *Configurer){
		"legacy": legacyFixture, "switchdev": switchdevFixture,
	} {
		_, nlops, configurer := fixture()
		state, err := configurer.Plan(Spec{PCIAddress: testVF}, &ClaimParams{MAC: "02:00:00:aa:bb:cc", Vlan: ptr.To(100)}, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(nlops.calls) != 0 {
			t.Errorf("%s: Plan changed the host: %v", name, nlops.calls)
		}
		if state.OrigMAC != "aa:bb:cc:dd:ee:03" || state.Vlan != 100 {
			t.Errorf("%s: plan %#v lacks the originals or the target VLAN", name, state)
		}
	}
}

func TestApplySwitchdevVlanValidation(t *testing.T) {
	// A VLAN the uplink does not carry is rejected.
	_, _, configurer := switchdevFixture()
	_, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{Vlan: ptr.To(200)}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not carry it") {
		t.Fatalf("expected uplink VLAN rejection, got %v", err)
	}

	// VLAN 0 resolves to the uplink's egress-untagged VID.
	_, nlops, configurer := switchdevFixture()
	state, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Vlan != 1 {
		t.Fatalf("expected VLAN resolved to 1, got %d", state.Vlan)
	}
	nlops.hasCall(t, "vlan-add enp8s0f0r3 1 pvid=true untagged=true")
}

func TestApplySwitchdevForeignMaster(t *testing.T) {
	_, nlops, configurer := switchdevFixture()
	nlops.addLink("br-other", "bridge", 30, 0, 1500)
	nlops.addLink("enp8s0f0r3", "device", 20, 30, 1500) // enslaved elsewhere

	_, err := planAndApply(configurer, Spec{PCIAddress: testVF}, &ClaimParams{}, nil)
	if err == nil || !strings.Contains(err.Error(), "already enslaved") {
		t.Fatalf("expected foreign-master rejection, got %v", err)
	}
}

func TestValidateIOMMUGroup(t *testing.T) {
	sysfs := &fakeSysfs{
		driver: map[string]string{
			"0000:08:00.2": "vfio-pci",
			"0000:08:00.3": "vfio-pci",
			"0000:08:00.4": "mlx5_core",
			"0000:00:01.0": "pcieport",
		},
		groupDevices: map[int][]string{
			42: {"0000:08:00.2", "0000:08:00.3", "0000:00:01.0"},
			43: {"0000:08:00.2", "0000:08:00.4"},
			44: {"0000:08:00.2", "0000:ff:00.0"}, // unbound sibling
		},
	}
	configurer := NewConfigurer(sysfs, newFakeNetlink(), "/tmp")

	if err := configurer.ValidateIOMMUGroup(42, "0000:08:00.2"); err != nil {
		t.Fatalf("viable group rejected: %v", err)
	}
	if err := configurer.ValidateIOMMUGroup(44, "0000:08:00.2"); err != nil {
		t.Fatalf("group with unbound sibling rejected: %v", err)
	}
	if err := configurer.ValidateIOMMUGroup(43, "0000:08:00.2"); err == nil ||
		!strings.Contains(err.Error(), "not viable") {
		t.Fatalf("expected non-viable group error, got %v", err)
	}
}

func TestFormatVlanRanges(t *testing.T) {
	vlans := []*nl.BridgeVlanInfo{
		{Vid: 5},
		{Vid: 1, Flags: nl.BRIDGE_VLAN_INFO_PVID | nl.BRIDGE_VLAN_INFO_UNTAGGED},
		{Vid: 3}, {Vid: 2}, {Vid: 100},
	}
	out, truncated := formatVlanRanges(vlans)
	if out != "1-3,5,100" || truncated {
		t.Fatalf("unexpected ranges %q truncated=%v", out, truncated)
	}
	if out, truncated = formatVlanRanges(nil); out != "" || truncated {
		t.Fatalf("unexpected empty-set result %q truncated=%v", out, truncated)
	}

	// 50 non-contiguous VIDs cannot fit 64 chars: truncated at a range boundary.
	long := []*nl.BridgeVlanInfo{}
	for vid := 101; vid < 200; vid += 2 {
		long = append(long, &nl.BridgeVlanInfo{Vid: uint16(vid)})
	}
	out, truncated = formatVlanRanges(long)
	if !truncated {
		t.Fatalf("expected truncation, got %q", out)
	}
	if len(out) > maxVlanAttrLen || strings.HasSuffix(out, ",") || out == "" {
		t.Fatalf("malformed truncated ranges %q", out)
	}

	if vid, ok := untaggedVid(vlans); !ok || vid != 1 {
		t.Fatalf("unexpected untagged VID %d ok=%v", vid, ok)
	}
	if _, ok := untaggedVid(long); ok {
		t.Fatal("pure trunk must have no untagged VID")
	}

	if got := vlanSummary(vlans); got != "1-3,5,100 (1 untagged)" {
		t.Fatalf("unexpected summary %q", got)
	}
	if got := vlanSummary(nil); got != "no VLANs" {
		t.Fatalf("unexpected empty summary %q", got)
	}
}

// planAndApply runs the two prepare phases the driver runs.
func planAndApply(c *Configurer, spec Spec, params *ClaimParams, prior *HostState) (*HostState, error) {
	state, err := c.Plan(spec, params, prior)
	if err != nil {
		return nil, err
	}
	return state, c.Apply(state, params)
}
