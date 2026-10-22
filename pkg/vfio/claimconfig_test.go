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

func result(request string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{Request: request}
}

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
	params, err := ParseClaimParams(claim, testDriver, result("nic"))
	if err != nil {
		t.Fatal(err)
	}
	if params.MAC != "02:00:00:aa:bb:cc" || params.Vlan == nil || *params.Vlan != 1234 {
		t.Fatalf("unexpected params: %#v", params)
	}

	// Config for another request is ignored.
	params, err = ParseClaimParams(claim, testDriver, result("other"))
	if err != nil || len(params.setFields()) > 0 {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}

	// Config of another driver is ignored.
	params, err = ParseClaimParams(claimWithConfig("other.driver", "nic", `{"mac":"x"}`), testDriver, result("nic"))
	if err != nil || len(params.setFields()) > 0 {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}

	// No allocation at all.
	params, err = ParseClaimParams(&resourceapi.ResourceClaim{}, testDriver, result("nic"))
	if err != nil || len(params.setFields()) > 0 {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}

	// Empty parameters payload.
	params, err = ParseClaimParams(claimWithConfig(testDriver, "nic", ""), testDriver, result("nic"))
	if err != nil || len(params.setFields()) > 0 {
		t.Fatalf("expected empty params, got %#v, %v", params, err)
	}
}

// A parent request's config applies to its subrequests; two configs of this
// driver for one request are refused.
func TestParseClaimParamsMatching(t *testing.T) {
	claim := claimWithConfig(testDriver, "nic", `{"vlan":7}`)
	params, err := ParseClaimParams(claim, testDriver, result("nic/fast"))
	if err != nil || params.Vlan == nil || *params.Vlan != 7 {
		t.Fatalf("subrequest params = %#v, %v; want the parent request's vlan 7", params, err)
	}
	claim.Status.Allocation.Devices.Config = append(claim.Status.Allocation.Devices.Config,
		claimWithConfig(testDriver, "", `{"vlan":8}`).Status.Allocation.Devices.Config...)
	if _, err := ParseClaimParams(claim, testDriver, result("nic")); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("two configs: err = %v, want an ambiguity error", err)
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
		{"removed option", `{"spoofchk":"off"}`, "unknown field"},
		{"vlan 0", `{"vlan":0}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseClaimParams(claimWithConfig(testDriver, "nic", tc.parameters), testDriver, result("nic"))
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
