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

package metadata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

const testDriver = "vfio.petasus.io"

func testWriter(t *testing.T) *Writer {
	return &Writer{DriverName: testDriver, HostRoot: t.TempDir()}
}

func testDevice() Device {
	return Device{
		Driver: testDriver,
		Pool:   "node-1",
		Name:   "pci-0000-08-00-2",
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"resource.kubernetes.io/pciBusID": {StringValue: ptr.To("0000:08:00.2")},
		},
	}
}

func TestWriteAndRemove(t *testing.T) {
	w := testWriter(t)
	ref := ClaimRef{Namespace: "proj", Name: "vm-nic-net", UID: "uid-1"}

	path, err := w.Write(ref, "nic", []Device{testDevice()})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(w.HostRoot, "dra-device-metadata", "proj_vm-nic-net", "nic", "metadata.json")
	if path != want {
		t.Fatalf("unexpected host path %q, want %q", path, want)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md := &DeviceMetadata{}
	if err := json.Unmarshal(raw, md); err != nil {
		t.Fatal(err)
	}
	if md.APIVersion != APIVersionV1Alpha1 || md.Kind != Kind {
		t.Fatalf("unexpected type meta: %#v", md.TypeMeta)
	}
	if len(md.Requests) != 1 || md.Requests[0].Name != "nic" || len(md.Requests[0].Devices) != 1 {
		t.Fatalf("unexpected requests: %#v", md.Requests)
	}
	attr := md.Requests[0].Devices[0].Attributes["resource.kubernetes.io/pciBusID"]
	if attr.StringValue == nil || *attr.StringValue != "0000:08:00.2" {
		t.Fatalf("pciBusID attribute lost: %#v", md.Requests[0].Devices[0].Attributes)
	}

	// The container path is what KubeVirt's virt-launcher resolves.
	container := w.ContainerPath(ref, "nic")
	if container != "/var/run/kubernetes.io/dra-device-attributes/resourceclaims/vm-nic-net/nic/vfio.petasus.io-metadata.json" {
		t.Fatalf("unexpected container path %q", container)
	}

	if err := w.RemoveClaim("proj", "vm-nic-net"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("metadata file not removed: %v", err)
	}
	// Removing again is a no-op.
	if err := w.RemoveClaim("proj", "vm-nic-net"); err != nil {
		t.Fatal(err)
	}
}

func TestContainerPathTemplateGenerated(t *testing.T) {
	w := testWriter(t)
	ref := ClaimRef{Namespace: "proj", Name: "generated-abc123", UID: "uid-1", PodClaimName: ptr.To("nic-net")}
	container := w.ContainerPath(ref, "nic")
	if container != "/var/run/kubernetes.io/dra-device-attributes/resourceclaimtemplates/nic-net/nic/vfio.petasus.io-metadata.json" {
		t.Fatalf("unexpected container path %q", container)
	}
}
