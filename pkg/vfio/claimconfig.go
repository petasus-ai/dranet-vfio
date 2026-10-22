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
	"bytes"
	"encoding/json"
	"fmt"
	"net"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
)

// ClaimParams is the flat opaque-config schema of this driver. Both fields
// apply to Ethernet SR-IOV VFs only; setting either for a passthrough device
// (Ethernet PF, any InfiniBand device) fails prepare.
type ClaimParams struct {
	// MAC is the VF admin MAC, set via the PF.
	MAC string `json:"mac,omitempty"`

	// Vlan is the access VLAN. 0 (or omitted) means "no VLAN": untagged in
	// legacy mode, resolved to the uplink's egress-untagged VID in
	// switchdev mode.
	Vlan *int `json:"vlan,omitempty"`
}

// setFields names the parameters that are set, for error messages.
func (p *ClaimParams) setFields() []string {
	var set []string
	if p.MAC != "" {
		set = append(set, "mac")
	}
	if p.Vlan != nil {
		set = append(set, "vlan")
	}
	return set
}

// ParseClaimParams extracts and validates this driver's opaque parameters for
// one allocation result: the config entry of this driver that applies to the
// result's request (or its parent request). No entry yields empty
// parameters, which is valid (passthrough devices carry none); two entries
// are ambiguous and fail.
func ParseClaimParams(claim *resourceapi.ResourceClaim, driverName string, result resourceapi.DeviceRequestAllocationResult) (*ClaimParams, error) {
	params := &ClaimParams{}
	if claim.Status.Allocation == nil {
		return params, nil
	}
	var raw []byte
	for _, config := range resourceclaim.ConfigForResult(claim.Status.Allocation.Devices.Config, result) {
		if config.Opaque.Driver != driverName || len(config.Opaque.Parameters.Raw) == 0 {
			continue
		}
		if raw != nil {
			return nil, fmt.Errorf("more than one %s config applies to request %s", driverName, result.Request)
		}
		raw = config.Opaque.Parameters.Raw
	}
	if raw == nil {
		return params, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(params); err != nil {
		return nil, fmt.Errorf("failed to parse opaque parameters for request %s: %w", result.Request, err)
	}
	if err := params.validate(); err != nil {
		return nil, fmt.Errorf("invalid opaque parameters for request %s: %w", result.Request, err)
	}
	return params, nil
}

func (p *ClaimParams) validate() error {
	if p.MAC != "" {
		if err := validateMAC(p.MAC); err != nil {
			return err
		}
	}
	// 4095 is reserved and 0 means "no VLAN" (IEEE 802.1Q).
	if p.Vlan != nil && (*p.Vlan < 0 || *p.Vlan > 4094) {
		return fmt.Errorf("VLAN ID must be 0-4094, got %d", *p.Vlan)
	}
	return nil
}

// validateMAC rejects addresses the driver will not accept as a VF admin
// MAC. mlx5's mlx5_esw_set_vport_mac_locked() returns -EINVAL for any
// multicast address (the group bit in the first octet), and the all-zero
// address is the "clear the admin MAC" sentinel rather than a usable
// address. Catching both here turns an opaque EINVAL from netlink into a
// pointed configuration error.
func validateMAC(mac string) error {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("invalid MAC address %q: %w", mac, err)
	}
	if len(hw) != 6 {
		return fmt.Errorf("invalid MAC address %q: expected a 48-bit address, got %d bytes", mac, len(hw))
	}
	if hw[0]&0x01 != 0 {
		return fmt.Errorf("invalid MAC address %q: multicast/broadcast addresses are rejected by the kernel as VF admin MACs", mac)
	}
	if hw.String() == "00:00:00:00:00:00" {
		return fmt.Errorf("invalid MAC address %q: the all-zero address clears the VF admin MAC and cannot be assigned", mac)
	}
	return nil
}
