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

// Package metadata writes the KEP-5304 device metadata files that KubeVirt
// reads to turn an allocated DRA device into a libvirt <hostdev>.
//
// Kubernetes 1.35 has no kubelet support for this: k8s.io/dynamic-resource-
// allocation only grew the api/metadata and devicemetadata packages in 1.36.
// That does not block anything, because the mechanism is entirely driver
// side. The 1.36 helper writes the file itself, writes a CDI spec that
// bind-mounts it, and adds that CDI device to the prepare response; kubelet
// only forwards CDI device IDs, which it has done since DRA went GA. This
// package reproduces that behaviour so the same driver works on 1.35.
//
// The types mirror kubevirt.io/kubevirt/pkg/dra/metadata (v1.9.0), which in
// turn mirrors k8s.io/dynamic-resource-allocation/api/metadata/v1alpha1. They
// are duplicated rather than imported because importing the KubeVirt module
// would pull its entire dependency tree into a node agent; the round trip
// test in this package pins the two together by decoding this package's
// output with KubeVirt's own reader.
package metadata

import (
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The in-container directory layout. These strings are consumer facing API:
// KubeVirt's virt-launcher builds the exact same paths, so they must not
// change.
//
// Source: k8s.io/dynamic-resource-allocation/api/metadata/types.go (v0.36.3)
// and kubevirt.io/kubevirt/pkg/dra/utils.go (v1.9.0).
const (
	// ContainerDir is where metadata files appear inside the container.
	ContainerDir = "/var/run/kubernetes.io/dra-device-attributes"

	// ResourceClaimsSubDir holds claims referenced by name from the pod
	// spec, keyed by the ResourceClaim's own name.
	ResourceClaimsSubDir = "resourceclaims"

	// ResourceClaimTemplatesSubDir holds claims generated from a template,
	// keyed by the pod-local claim name. The generated ResourceClaim name is
	// not predictable from the pod spec, so the pod-local name is used.
	ResourceClaimTemplatesSubDir = "resourceclaimtemplates"

	// FileSuffix is appended to the driver name to form the file name, so
	// that several drivers serving one request do not collide.
	FileSuffix = "-metadata.json"
)

const (
	// APIVersionV1Alpha1 is the only version KubeVirt v1.9.0 understands.
	APIVersionV1Alpha1 = "metadata.resource.k8s.io/v1alpha1"

	// Kind is the object kind written into every encoded object.
	Kind = "DeviceMetadata"
)

// PodClaimNameAnnotation is set by the resourceclaim controller on every
// ResourceClaim generated from a ResourceClaimTemplate, and holds the
// pod-local claim name.
//
// k8s.io/api promoted this to resourceapi.PodResourceClaimAnnotation in 1.36
// only; the annotation itself is written by kube-controller-manager in 1.35
// as well (kubernetes/kubernetes release-1.35,
// pkg/controller/resourceclaim/controller.go).
const PodClaimNameAnnotation = "resource.kubernetes.io/pod-claim-name"

// DeviceMetadata is the metadata for one ResourceClaim, as written to a
// single request's metadata file.
type DeviceMetadata struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// PodClaimName is present only for template-generated claims.
	PodClaimName *string `json:"podClaimName,omitempty"`

	// Requests holds exactly one entry: the request this file describes.
	Requests []Request `json:"requests,omitempty"`
}

// Request is the allocation result for one request of a ResourceClaim.
type Request struct {
	// Name is the request reference. For a request using firstAvailable
	// subrequests this carries "<request>/<subrequest>", matching
	// DeviceRequestAllocationResult.Request.
	Name string `json:"name"`

	// Devices holds the devices allocated to the request. KubeVirt requires
	// exactly one.
	Devices []Device `json:"devices,omitempty"`
}

// Device describes one allocated device.
type Device struct {
	// Driver is the DRA driver name.
	Driver string `json:"driver"`

	// Pool is the ResourceSlice pool the device came from.
	Pool string `json:"pool"`

	// Name is the device's name within the pool.
	Name string `json:"name"`

	// Attributes uses the same encoding as a ResourceSlice. KubeVirt reads
	// "resource.kubernetes.io/pciBusID" and "mdevUUID" from here.
	Attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute `json:"attributes,omitempty"`

	// NetworkData is populated for network devices only; this driver never
	// sets it.
	NetworkData *resourceapi.NetworkDeviceData `json:"networkData,omitempty"`
}
