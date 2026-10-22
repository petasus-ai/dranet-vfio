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

// This file is a port of KubeVirt's metadata reader, taken from
// kubevirt.io/kubevirt v1.9.0:
//
//   - pkg/dra/utils.go: resolveClaimMetadata, readMetadataFromDir,
//     readMetadataFile, decodeMetadataFromStream, resolveDevice,
//     GetPCIAddressForClaim, GetMDevUUIDForClaim
//   - pkg/dra/metadata/types.go: PCIBusIDAttribute, MDevUUIDAttribute,
//     IsSupportedAPIVersion
//
// It exists so that the writer in this package is verified against the code
// that will actually consume its output, rather than against an
// interpretation of it. There is no hardware or cluster available to run a
// real end-to-end test, so this is the closest substitute; keep it a faithful
// port and update it deliberately when the pinned KubeVirt version changes.
//
// Deviations from the original, all mechanical:
//   - the metadata types are this package's, which mirror KubeVirt's field
//     for field
//   - v1.VirtualMachineInstanceResourceClaim is replaced by the local
//     vmiResourceClaim struct with the same three fields
//   - logging calls are dropped

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// vmiResourceClaim mirrors v1.VirtualMachineInstanceResourceClaim.
type vmiResourceClaim struct {
	Name                      string
	ResourceClaimName         *string
	ResourceClaimTemplateName *string
}

const (
	kvResourceClaimsSubdir         = "resourceclaims"
	kvResourceClaimTemplatesSubdir = "resourceclaimtemplates"
	kvMetadataFileSuffix           = "-metadata.json"

	kvPCIBusIDAttribute = "resource.kubernetes.io/pciBusID"
	kvMDevUUIDAttribute = "mdevUUID"
)

func kvIsSupportedAPIVersion(v string) bool {
	return v == "metadata.resource.k8s.io/v1alpha1"
}

func kvGetPCIAddressForClaim(basePath string, resourceClaims []vmiResourceClaim, claimRefName, requestName string) (string, error) {
	device, err := kvResolveDevice(basePath, resourceClaims, claimRefName, requestName)
	if err != nil {
		return "", err
	}
	if attr, ok := device.Attributes[kvPCIBusIDAttribute]; ok {
		if attr.StringValue != nil && *attr.StringValue != "" {
			return *attr.StringValue, nil
		}
	}
	return "", fmt.Errorf("pciBusID not found for claim %q request %q", claimRefName, requestName)
}

func kvGetMDevUUIDForClaim(basePath string, resourceClaims []vmiResourceClaim, claimRefName, requestName string) (string, error) {
	device, err := kvResolveDevice(basePath, resourceClaims, claimRefName, requestName)
	if err != nil {
		return "", err
	}
	if attr, ok := device.Attributes[kvMDevUUIDAttribute]; ok {
		if attr.StringValue != nil && *attr.StringValue != "" {
			return *attr.StringValue, nil
		}
	}
	return "", fmt.Errorf("mdevUUID not found for claim %q request %q", claimRefName, requestName)
}

func kvResolveDevice(basePath string, resourceClaims []vmiResourceClaim, claimRefName, requestName string) (*Device, error) {
	md, err := kvResolveClaimMetadata(basePath, resourceClaims, claimRefName, requestName)
	if err != nil {
		return nil, err
	}

	for _, req := range md.Requests {
		if req.Name == requestName {
			if len(req.Devices) == 0 {
				return nil, fmt.Errorf("request %q has no devices", requestName)
			}
			if len(req.Devices) > 1 {
				return nil, fmt.Errorf(
					"request %q has %d devices but KubeVirt only supports exactly one device per request (count > 1 is not supported)",
					requestName, len(req.Devices))
			}
			return &req.Devices[0], nil
		}
	}
	return nil, fmt.Errorf("request %q not found in metadata for claim %q", requestName, md.Name)
}

func kvResolveClaimMetadata(basePath string, resourceClaims []vmiResourceClaim, claimRefName, requestName string) (*DeviceMetadata, error) {
	for _, rc := range resourceClaims {
		if rc.Name != claimRefName {
			continue
		}
		if rc.ResourceClaimName != nil && *rc.ResourceClaimName != "" {
			return kvReadMetadataFromDir(filepath.Join(basePath, kvResourceClaimsSubdir), *rc.ResourceClaimName, requestName)
		}
		return kvReadMetadataFromDir(filepath.Join(basePath, kvResourceClaimTemplatesSubdir), rc.Name, requestName)
	}
	return nil, fmt.Errorf("metadata not found for claim %q", claimRefName)
}

func kvReadMetadataFromDir(basePath, claimName, requestName string) (*DeviceMetadata, error) {
	pattern := filepath.Join(basePath, claimName, requestName, "*"+kvMetadataFileSuffix)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to glob metadata for claim %q request %q: %w", claimName, requestName, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("failed to read metadata for claim %q request %q: no files matching %s", claimName, requestName, pattern)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf(
			"found %d metadata files for claim %q request %q but KubeVirt only supports exactly one driver per request",
			len(matches), claimName, requestName)
	}
	return kvReadMetadataFile(matches[0])
}

func kvReadMetadataFile(path string) (*DeviceMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening metadata file %q: %w", path, err)
	}
	defer f.Close()

	md, err := kvDecodeMetadataFromStream(json.NewDecoder(f))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return md, nil
}

func kvDecodeMetadataFromStream(dec *json.Decoder) (*DeviceMetadata, error) {
	var unknownVersions []string
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("read object from metadata stream: %w", err)
		}

		var peek struct {
			APIVersion string `json:"apiVersion"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			return nil, fmt.Errorf("decode metadata object: %w", err)
		}
		if peek.APIVersion == "" {
			return nil, fmt.Errorf("decode metadata object: missing apiVersion")
		}

		if !kvIsSupportedAPIVersion(peek.APIVersion) {
			unknownVersions = append(unknownVersions, peek.APIVersion)
			continue
		}

		var md DeviceMetadata
		if err := json.Unmarshal(raw, &md); err != nil {
			return nil, fmt.Errorf("decode %s: %w", peek.APIVersion, err)
		}
		return &md, nil
	}
	if len(unknownVersions) == 0 {
		return nil, fmt.Errorf("no metadata objects found in stream")
	}
	return nil, fmt.Errorf("no compatible metadata version found in stream (unknown versions: %s)", strings.Join(unknownVersions, ", "))
}
