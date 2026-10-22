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

package cdi

import (
	"encoding/json"
	"os"
	"testing"

	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

func TestWriteAndRemove(t *testing.T) {
	w := &Writer{DriverName: "vfio.petasus.io", SpecDir: t.TempDir()}
	name := DeviceName("uid-1", "nic", "pci-0000-01-08-2")
	mode := os.FileMode(0o660)
	uid := uint32(107)
	dev := cdispec.Device{Name: name, ContainerEdits: cdispec.ContainerEdits{
		DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/vfio/12", HostPath: "/dev/vfio/12", FileMode: &mode, Permissions: "rw", UID: &uid, GID: &uid}},
	}}
	if err := w.Write("uid-1", []cdispec.Device{dev}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := w.DeviceID(name), "vfio.petasus.io/vfio=uid-1_nic_pci-0000-01-08-2"; got != want {
		t.Errorf("DeviceID = %q, want %q", got, want)
	}

	data, err := os.ReadFile(w.SpecPath("uid-1"))
	if err != nil {
		t.Fatalf("reading spec: %v", err)
	}
	var spec cdispec.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decoding spec: %v", err)
	}
	// Device node host paths need CDI 0.5.0.
	if spec.Version != "0.5.0" || spec.Kind != "vfio.petasus.io/vfio" || len(spec.Devices) != 1 {
		t.Fatalf("unexpected spec header: %+v", spec)
	}
	node := spec.Devices[0].ContainerEdits.DeviceNodes[0]
	if node.UID == nil || *node.UID != 107 || node.FileMode == nil || *node.FileMode != 0o660 {
		t.Errorf("device node ownership not kept: %+v", node)
	}

	if err := w.Remove("uid-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(w.SpecPath("uid-1")); !os.IsNotExist(err) {
		t.Errorf("spec still present after Remove: %v", err)
	}
	if err := w.Remove("uid-1"); err != nil {
		t.Errorf("Remove of a missing spec: %v", err)
	}
}
