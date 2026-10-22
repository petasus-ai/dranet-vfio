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
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/containerd/nri/pkg/api"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/api/metadata"
	"k8s.io/dynamic-resource-allocation/api/metadata/install"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"sigs.k8s.io/dranet/pkg/vfio/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

func testVfioDeviceConfig(cdiDeviceID string) DeviceConfig {
	return DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "vm-nic"},
		CarryDeviceConfig: CarryDeviceConfig{
			VfioDevices: []LinuxDevice{
				{Path: "/dev/vfio/vfio", Type: "c", Major: 10, Minor: 196, FileMode: vfioDevFileMode, UID: qemuUID, GID: qemuGID},
				{Path: "/dev/vfio/12", Type: "c", Major: 509, Minor: 1, FileMode: vfioDevFileMode, UID: qemuUID, GID: qemuGID},
			},
			CDIDeviceID: cdiDeviceID,
		},
	}
}

// The runtime applies the claim's CDI device; NRI must not inject the vfio
// devices a second time.
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
	dev := vfioCDIDevice("uid_nic_dev", cfg.VfioDevices)
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
}

func TestUnprepareRemovesCDISpec(t *testing.T) {
	np := &NetworkDriver{
		podConfigStore: mustNewPodConfigStore(),
		carryState: carryState{
			vfioCDI: &cdi.Writer{DriverName: "vfio.petasus.io", SpecDir: t.TempDir()},
		},
	}
	if err := np.vfioCDI.Write("claim-uid", []cdispec.Device{{Name: "claim-uid_nic_dev"}}); err != nil {
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
	for _, key := range []string{"vfioPciAddress", "vfioDevices", "cdiDeviceID"} {
		if _, ok := top[key]; !ok {
			t.Errorf("key %q is not at the top level of %s", key, data)
		}
	}
}

// KubeVirt v1.9.1 reads the first object of the metadata stream whose
// apiVersion it knows (v1alpha1 only) and takes resource.kubernetes.io/pciBusID
// from the request's single device (pkg/dra/utils.go). The stream is encoded
// as the kubeletplugin helper encodes it, with this driver's versions.
func TestVfioMetadataReadsInKubeVirt(t *testing.T) {
	codecs := serializer.NewCodecFactory(install.NewScheme())
	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), runtime.ContentTypeJSON)
	if !ok {
		t.Fatal("no JSON serializer")
	}
	pci := "0000:01:08.2"
	dm := &metadata.DeviceMetadata{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vm-nic", UID: "claim-uid", Generation: 1},
		Requests: []metadata.DeviceMetadataRequest{{Name: "nic", Devices: []metadata.Device{{
			Driver: "vfio.petasus.io", Pool: "node1", Name: "pci-0000-01-08-2",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				deviceattribute.StandardDeviceAttributePCIBusID: {StringValue: &pci},
			},
		}}}},
	}
	var stream bytes.Buffer
	for _, gv := range vfioMetadataVersions {
		data, err := runtime.Encode(codecs.EncoderForVersion(info.PrettySerializer, gv), dm)
		if err != nil {
			t.Fatalf("encode %s: %v", gv, err)
		}
		stream.Write(data)
		stream.WriteByte('\n')
	}

	// KubeVirt's reader, reduced to the decode and the PCI lookup.
	type kvMetadata struct {
		Requests []struct {
			Name    string `json:"name"`
			Devices []struct {
				Attributes map[string]resourceapi.DeviceAttribute `json:"attributes"`
			} `json:"devices"`
		} `json:"requests"`
	}
	dec := json.NewDecoder(&stream)
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatal(err)
		}
		var peek struct {
			APIVersion string `json:"apiVersion"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			t.Fatal(err)
		}
		if peek.APIVersion != "metadata.resource.k8s.io/v1alpha1" {
			continue
		}
		var md kvMetadata
		if err := json.Unmarshal(raw, &md); err != nil {
			t.Fatal(err)
		}
		if len(md.Requests) != 1 || md.Requests[0].Name != "nic" || len(md.Requests[0].Devices) != 1 {
			t.Fatalf("requests = %+v, want request nic with one device", md.Requests)
		}
		got := md.Requests[0].Devices[0].Attributes["resource.kubernetes.io/pciBusID"].StringValue
		if got == nil || *got != pci {
			t.Fatalf("pciBusID = %v, want %s", got, pci)
		}
		return
	}
	t.Fatal("no v1alpha1 object in the metadata stream")
}
