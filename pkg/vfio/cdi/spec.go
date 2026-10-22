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
// prepared vfio-pci device's /dev/vfio nodes to a pod.
//
// Kubelet caches the CDI device IDs a prepare returns per claim and injects
// them into every pod that references the claim on the node, including a pod
// that reuses a claim already prepared for another pod and so never reaches
// the driver.
package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"sigs.k8s.io/dranet/internal/atomicfile"
)

// DefaultSpecDir is where container runtimes look for generated CDI specs.
const DefaultSpecDir = "/var/run/cdi"

// Class is the CDI class of this driver's devices; the CDI kind is
// "<driver name>/<class>".
const Class = "vfio"

const specFilePerms = os.FileMode(0o644)

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

// Write atomically replaces the CDI spec of one claim, at the oldest spec
// version that carries what the devices use. Devices are sorted so a
// repeated prepare produces an identical file.
func (w *Writer) Write(claimUID string, devices []cdispec.Device) error {
	if w.DriverName == "" || claimUID == "" {
		return fmt.Errorf("CDI spec needs a driver name and a claim UID")
	}
	spec := &cdispec.Spec{Kind: w.Kind(), Devices: append([]cdispec.Device(nil), devices...)}
	sort.Slice(spec.Devices, func(i, j int) bool { return spec.Devices[i].Name < spec.Devices[j].Name })
	version, err := cdispec.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("CDI spec version: %w", err)
	}
	spec.Version = version
	data, err := json.MarshalIndent(spec, "", "  ")
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
