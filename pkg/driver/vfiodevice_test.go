/*
Copyright The Petasus Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"sigs.k8s.io/dranet/pkg/vfio/cdi"
	"sigs.k8s.io/dranet/pkg/vfio/metadata"
)

func testVfioDeviceConfig(cdiDeviceID string) DeviceConfig {
	return DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "vm-nic"},
		CarryDeviceConfig: CarryDeviceConfig{
			VfioDevices: []LinuxDevice{
				{Path: "/dev/vfio/vfio", Type: "c", Major: 10, Minor: 196, FileMode: vfioDevFileMode, UID: qemuUID, GID: qemuGID},
				{Path: "/dev/vfio/12", Type: "c", Major: 509, Minor: 1, FileMode: vfioDevFileMode, UID: qemuUID, GID: qemuGID},
			},
			MetadataMount: &BindMount{HostPath: "/host/metadata.json", ContainerPath: "/ctr/metadata.json"},
			CDIDeviceID:   cdiDeviceID,
		},
	}
}

// The runtime applies the claim's CDI device; NRI must not inject the vfio
// devices or the metadata mount a second time.
func TestCreateContainerLeavesVfioToCDI(t *testing.T) {
	np := &NetworkDriver{podConfigStore: mustNewPodConfigStore()}
	podUID := types.UID("pod-a")
	if err := np.podConfigStore.SetDeviceConfig(podUID, "pci-0000-01-08-2", testVfioDeviceConfig("vfio.petasus.io/vfio=uid_nic_dev")); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}
	pod := &api.PodSandbox{Uid: string(podUID), Name: "virt-launcher", Namespace: "ns"}
	adjust, _, err := np.CreateContainer(context.Background(), pod, &api.Container{Name: "compute"})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if adjust != nil && (adjust.Linux != nil && len(adjust.Linux.Devices) > 0 || len(adjust.Mounts) > 0) {
		t.Errorf("NRI injected %+v, want nothing", adjust)
	}
}

func TestVfioCDIDevice(t *testing.T) {
	cfg := testVfioDeviceConfig("")
	dev := vfioCDIDevice("uid_nic_dev", cfg.VfioDevices, *cfg.MetadataMount)
	if dev.Name != "uid_nic_dev" {
		t.Errorf("name = %q", dev.Name)
	}
	if len(dev.ContainerEdits.DeviceNodes) != 2 {
		t.Fatalf("got %d device nodes, want 2", len(dev.ContainerEdits.DeviceNodes))
	}
	for _, n := range dev.ContainerEdits.DeviceNodes {
		if n.HostPath != n.Path || n.Permissions != "rw" {
			t.Errorf("device node %+v: want hostPath = path and rw permissions", n)
		}
		if n.UID == nil || *n.UID != qemuUID || n.GID == nil || *n.GID != qemuGID || n.FileMode == nil || *n.FileMode != os.FileMode(vfioDevFileMode) {
			t.Errorf("device node %+v: want qemu-owned 0660", n)
		}
	}
	mounts := dev.ContainerEdits.Mounts
	if len(mounts) != 1 || mounts[0].HostPath != "/host/metadata.json" || mounts[0].ContainerPath != "/ctr/metadata.json" {
		t.Errorf("mounts = %+v", mounts)
	}
}

func TestUnprepareRemovesCDISpec(t *testing.T) {
	np := &NetworkDriver{
		podConfigStore: mustNewPodConfigStore(),
		carryState: carryState{
			vfioMetadata: &metadata.Writer{DriverName: "vfio.petasus.io", HostRoot: t.TempDir()},
			vfioCDI:      &cdi.Writer{DriverName: "vfio.petasus.io", SpecDir: t.TempDir()},
		},
	}
	if err := np.vfioCDI.Write("claim-uid", []cdi.Device{{Name: "claim-uid_nic_dev"}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	claim := kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vm-nic"},
		UID:            "claim-uid",
	}
	if err := np.unprepareResourceClaim(context.Background(), claim); err != nil {
		t.Fatalf("unprepareResourceClaim: %v", err)
	}
	if _, err := os.Stat(np.vfioCDI.SpecPath("claim-uid")); !os.IsNotExist(err) {
		t.Errorf("CDI spec still present after unprepare: %v", err)
	}
}

// Node state DBs written before the carry fields moved into the embedded
// struct keep their top-level keys; they must decode unchanged.
func TestCarryDeviceConfigCheckpoint(t *testing.T) {
	want := testVfioDeviceConfig("vfio.petasus.io/vfio=uid_nic_dev")
	want.VfioPCIAddress = "0000:01:08.2"

	cp, err := newBoltCheckpointer(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("newBoltCheckpointer: %v", err)
	}
	defer cp.Close()
	if err := cp.Store("pod-1", "pci-0000-01-08-2", want); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := cp.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if !reflect.DeepEqual(got["pod-1"]["pci-0000-01-08-2"], want) {
		t.Errorf("roundtrip mismatch:\n got %#v\nwant %#v", got["pod-1"]["pci-0000-01-08-2"], want)
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"vfioPciAddress", "vfioDevices", "metadataMount", "cdiDeviceID"} {
		if _, ok := top[key]; !ok {
			t.Errorf("key %q is not at the top level of %s", key, data)
		}
	}
}
