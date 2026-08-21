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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/dranet/internal/atomicfile"
)

const (
	filePerms = os.FileMode(0o644)
	dirPerms  = os.FileMode(0o755)

	// hostSubDir is where metadata files live on the host, below the plugin's
	// data directory. It matches the layout the 1.36 kubeletplugin helper
	// uses so that a future migration to the upstream writer is a no-op.
	hostSubDir = "dra-device-metadata"
)

// ClaimRef identifies the ResourceClaim a metadata file belongs to.
type ClaimRef struct {
	Namespace string
	Name      string
	UID       types.UID

	// PodClaimName is the pod-local claim name for template-generated
	// claims, and nil for claims referenced by name.
	PodClaimName *string
}

// NewClaimRef derives a ClaimRef from an allocated ResourceClaim, reading the
// pod-local claim name from the annotation kube-controller-manager sets on
// template-generated claims.
func NewClaimRef(claim *resourceapi.ResourceClaim) ClaimRef {
	ref := ClaimRef{
		Namespace: claim.Namespace,
		Name:      claim.Name,
		UID:       claim.UID,
	}
	if v, ok := claim.Annotations[PodClaimNameAnnotation]; ok && v != "" {
		podClaimName := v
		ref.PodClaimName = &podClaimName
	}
	return ref
}

// IsTemplateGenerated reports whether the claim came from a
// ResourceClaimTemplate, which decides the in-container directory.
func (r ClaimRef) IsTemplateGenerated() bool {
	return r.PodClaimName != nil && *r.PodClaimName != ""
}

// Writer produces metadata files for one driver.
type Writer struct {
	// DriverName is this driver's name; it forms the file name.
	DriverName string

	// HostRoot is the plugin data directory. Files are written below
	// HostRoot/dra-device-metadata.
	HostRoot string
}

// FileName is the metadata file name for this driver.
func (w *Writer) FileName() string { return w.DriverName + FileSuffix }

// ClaimDir is the host directory holding every request's metadata for a
// claim. Removing it is how a claim is cleaned up.
func (w *Writer) ClaimDir(namespace, name string) string {
	return filepath.Join(w.HostRoot, hostSubDir, namespace+"_"+name)
}

// HostPath is where a request's metadata file lives on the host.
func (w *Writer) HostPath(namespace, name, requestName string) string {
	return filepath.Join(w.ClaimDir(namespace, name), requestName, "metadata.json")
}

// ContainerPath is where the file has to appear inside the container for
// KubeVirt to find it.
func (w *Writer) ContainerPath(ref ClaimRef, requestName string) string {
	if ref.IsTemplateGenerated() {
		return filepath.Join(ContainerDir, ResourceClaimTemplatesSubDir, *ref.PodClaimName, requestName, w.FileName())
	}
	return filepath.Join(ContainerDir, ResourceClaimsSubDir, ref.Name, requestName, w.FileName())
}

// Write records the devices allocated to one request and returns the host
// path of the file.
//
// requestRef is written into the file as-is so that the content matches what
// the 1.36 kubelet helper would produce; the file path uses the base request
// name, which is what KubeVirt looks up.
func (w *Writer) Write(ref ClaimRef, requestRef string, devices []Device) (string, error) {
	if w.DriverName == "" {
		return "", fmt.Errorf("driver name is required")
	}
	if ref.Name == "" || ref.Namespace == "" {
		return "", fmt.Errorf("claim namespace and name are required")
	}
	if requestRef == "" {
		return "", fmt.Errorf("request name is required")
	}

	data, err := Encode(w.buildMetadata(ref, requestRef, devices))
	if err != nil {
		return "", err
	}

	path := w.HostPath(ref.Namespace, ref.Name, BaseRequestName(requestRef))
	if err := os.MkdirAll(filepath.Dir(path), dirPerms); err != nil {
		return "", fmt.Errorf("creating metadata directory: %w", err)
	}
	if err := atomicfile.Write(path, data, filePerms); err != nil {
		return "", fmt.Errorf("writing metadata file %s: %w", path, err)
	}
	return path, nil
}

// RemoveClaim deletes every metadata file written for a claim. It is a no-op
// when nothing was written, so repeated unprepare calls are safe.
func (w *Writer) RemoveClaim(namespace, name string) error {
	dir := w.ClaimDir(namespace, name)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing metadata directory %s: %w", dir, err)
	}
	return nil
}

func (w *Writer) buildMetadata(ref ClaimRef, requestRef string, devices []Device) *DeviceMetadata {
	return &DeviceMetadata{
		TypeMeta: metav1.TypeMeta{
			Kind:       Kind,
			APIVersion: APIVersionV1Alpha1,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       ref.Name,
			Namespace:  ref.Namespace,
			UID:        ref.UID,
			Generation: 1,
		},
		PodClaimName: ref.PodClaimName,
		Requests: []Request{
			{Name: requestRef, Devices: devices},
		},
	}
}

// Encode serialises the metadata as the concatenated JSON stream the format
// prescribes: the same object once per supported API version, newest first.
// This driver supports a single version today, so the stream holds one
// object; consumers skip versions they do not recognise, which is what makes
// adding a version later safe.
func Encode(md *DeviceMetadata) ([]byte, error) {
	var buf bytes.Buffer
	data, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding metadata: %w", err)
	}
	buf.Write(data)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// BaseRequestName strips the subrequest part of a request reference.
//
// KubeVirt matches on the base name only, so a claim using firstAvailable
// subrequests will not resolve; see docs/CONTRACT.md.
func BaseRequestName(requestRef string) string {
	if idx := strings.Index(requestRef, "/"); idx >= 0 {
		return requestRef[:idx]
	}
	return requestRef
}
