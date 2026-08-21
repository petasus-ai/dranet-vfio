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
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const testDriver = "vfio.petasus.io"

func claimWithConfig(driver, request, parameters string) *resourceapi.ResourceClaim {
	claim := &resourceapi.ResourceClaim{}
	claim.Status.Allocation = &resourceapi.AllocationResult{}
	config := resourceapi.DeviceAllocationConfiguration{
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver:     driver,
				Parameters: runtime.RawExtension{Raw: []byte(parameters)},
			},
		},
	}
	if request != "" {
		config.Requests = []string{request}
	}
	claim.Status.Allocation.Devices.Config = append(claim.Status.Allocation.Devices.Config, config)
	return claim
}

func TestParseClaimParams(t *testing.T) {
	claim := claimWithConfig(testDriver, "nic", `{"mac":"02:00:00:aa:bb:cc","vlan":1234}`)
	params, err := ParseClaimParams(claim, testDriver, "nic")
	if err != nil {
		t.Fatal(err)
	}
	if params.MAC != "02:00:00:aa:bb:cc" || params.Vlan == nil || *params.Vlan != 1234 {
		t.Fatalf("unexpected params: %#v", params)
	}

	// Config for another request is ignored.
	params, err = ParseClaimParams(claim, testDriver, "other")
	if err != nil || !params.IsZero() {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}

	// Config of another driver is ignored.
	params, err = ParseClaimParams(claimWithConfig("other.driver", "nic", `{"mac":"x"}`), testDriver, "nic")
	if err != nil || !params.IsZero() {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}

	// No allocation at all.
	params, err = ParseClaimParams(&resourceapi.ResourceClaim{}, testDriver, "nic")
	if err != nil || !params.IsZero() {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}

	// Empty parameters payload.
	params, err = ParseClaimParams(claimWithConfig(testDriver, "nic", ""), testDriver, "nic")
	if err != nil || !params.IsZero() {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}
}

func TestParseClaimParamsValidation(t *testing.T) {
	tests := []struct {
		name       string
		parameters string
		wantErr    string
	}{
		{"unknown field", `{"vlanId":100}`, "unknown field"},
		{"bad mac", `{"mac":"not-a-mac"}`, "invalid MAC"},
		{"multicast mac", `{"mac":"01:00:5e:00:00:01"}`, "multicast"},
		{"zero mac", `{"mac":"00:00:00:00:00:00"}`, "all-zero"},
		{"vlan out of range", `{"vlan":4095}`, "VLAN ID must be 0-4094"},
		{"negative vlan", `{"vlan":-1}`, "VLAN ID must be 0-4094"},
		{"bad spoofchk", `{"spoofchk":"yes"}`, "spoofchk"},
		{"bad trust", `{"trust":"maybe"}`, "trust"},
		{"negative rate", `{"minTxRate":-5}`, "minTxRate"},
		{"min above max", `{"minTxRate":100,"maxTxRate":50}`, "must be <= maxTxRate"},
		{"max 0 uncaps", `{"minTxRate":100,"maxTxRate":0}`, ""},
		{"bad bridge", `{"bridge":"much-too-long-bridge-name"}`, "bridge"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClaimParams(claimWithConfig(testDriver, "nic", tc.parameters), testDriver, "nic")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
