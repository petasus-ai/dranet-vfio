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

// Package cdi writes the Container Device Interface specs that hand a
// prepared vfio-pci device to a pod: the /dev/vfio character devices and the
// KEP-5304 metadata file mount.
//
// Kubelet caches the CDI device IDs a prepare returns per claim and injects
// them into every pod that references the claim on the node, including a pod
// that reuses a claim already prepared for another pod and so never reaches
// the driver. The spec types are declared here instead of importing the CDI
// library: the driver emits only device nodes and file mounts.
package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/dranet/internal/atomicfile"
)

// SpecVersion is the CDI spec version emitted: the oldest that carries
// device node host paths.
const SpecVersion = "0.5.0"

// DefaultSpecDir is where container runtimes look for generated CDI specs.
const DefaultSpecDir = "/var/run/cdi"

// Class is the CDI class of this driver's devices; the CDI kind is
// "<driver name>/<class>".
const Class = "vfio"

const specFilePerms = os.FileMode(0o644)

// Spec is a CDI specification file.
type Spec struct {
	Version string   `json:"cdiVersion"`
	Kind    string   `json:"kind"`
	Devices []Device `json:"devices"`
}

// Device is one CDI device: a named bundle of container edits.
type Device struct {
	Name           string         `json:"name"`
	ContainerEdits ContainerEdits `json:"containerEdits"`
}

// ContainerEdits are the modifications applied to a container that requests
// the device.
type ContainerEdits struct {
	DeviceNodes []DeviceNode `json:"deviceNodes,omitempty"`
	Mounts      []Mount      `json:"mounts,omitempty"`
}

// DeviceNode exposes a host character device inside the container.
type DeviceNode struct {
	Path        string       `json:"path"`
	HostPath    string       `json:"hostPath,omitempty"`
	Type        string       `json:"type,omitempty"`
	Major       int64        `json:"major,omitempty"`
	Minor       int64        `json:"minor,omitempty"`
	FileMode    *os.FileMode `json:"fileMode,omitempty"`
	Permissions string       `json:"permissions,omitempty"`
	UID         *uint32      `json:"uid,omitempty"`
	GID         *uint32      `json:"gid,omitempty"`
}

// Mount bind-mounts a host path into the container.
type Mount struct {
	HostPath      string   `json:"hostPath"`
	ContainerPath string   `json:"containerPath"`
	Options       []string `json:"options,omitempty"`
}

// Writer manages this driver's CDI spec files, one per ResourceClaim.
type Writer struct {
	// DriverName is the CDI vendor, i.e. the DRA driver name.
	DriverName string

	// SpecDir is the directory the runtime scans; DefaultSpecDir when empty.
	SpecDir string
}

// Kind is the CDI kind this driver publishes under.
func (w *Writer) Kind() string { return w.DriverName + "/" + Class }

func (w *Writer) specDir() string {
	if w.SpecDir == "" {
		return DefaultSpecDir
	}
	return w.SpecDir
}

// SpecPath is the file holding every CDI device of one claim, so cleanup is
// a single unlink.
func (w *Writer) SpecPath(claimUID string) string {
	return filepath.Join(w.specDir(), fmt.Sprintf("%s_%s.json", w.DriverName, claimUID))
}

// DeviceName is the per-device CDI name; the claim UID keeps two claims with
// the same request and device names apart.
func DeviceName(claimUID, requestName, deviceName string) string {
	return claimUID + "_" + requestName + "_" + deviceName
}

// DeviceID is the fully qualified CDI device name returned to kubelet.
func (w *Writer) DeviceID(deviceName string) string {
	return w.Kind() + "=" + deviceName
}

// Write atomically replaces the CDI spec of one claim. Devices are sorted so
// a repeated prepare produces an identical file.
func (w *Writer) Write(claimUID string, devices []Device) error {
	if w.DriverName == "" || claimUID == "" {
		return fmt.Errorf("CDI spec needs a driver name and a claim UID")
	}
	sorted := append([]Device(nil), devices...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	data, err := json.MarshalIndent(Spec{Version: SpecVersion, Kind: w.Kind(), Devices: sorted}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding CDI spec: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(w.specDir(), 0o755); err != nil {
		return fmt.Errorf("creating CDI directory: %w", err)
	}
	return atomicfile.Write(w.SpecPath(claimUID), data, specFilePerms)
}

// Remove deletes a claim's CDI spec; a missing spec is not an error.
func (w *Writer) Remove(claimUID string) error {
	if err := os.Remove(w.SpecPath(claimUID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing CDI spec: %w", err)
	}
	return nil
}
