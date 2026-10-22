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
	"slices"

	resourceapi "k8s.io/api/resource/v1"
)

// ClaimParams is the flat opaque-config schema of this driver. All fields
// apply to Ethernet SR-IOV VFs only; setting any of them for a passthrough
// device (Ethernet PF, any InfiniBand device) fails prepare.
type ClaimParams struct {
	// MAC is the VF admin MAC, set via the PF.
	MAC string `json:"mac,omitempty"`

	// Vlan is the access VLAN. 0 (or omitted) means "no VLAN": untagged in
	// legacy mode, resolved to the uplink's egress-untagged VID in
	// switchdev mode.
	Vlan *int `json:"vlan,omitempty"`

	// SpoofChk/Trust toggle the VF flags ("on"/"off"); legacy mode only.
	SpoofChk string `json:"spoofchk,omitempty"`
	Trust    string `json:"trust,omitempty"`

	// MinTxRate/MaxTxRate cap the VF's transmit rate in Mbps.
	MinTxRate *int `json:"minTxRate,omitempty"`
	MaxTxRate *int `json:"maxTxRate,omitempty"`

	// Bridge overrides auto-detection of the Linux bridge for switchdev mode.
	Bridge string `json:"bridge,omitempty"`
}

// IsZero reports whether no parameter is set.
func (p *ClaimParams) IsZero() bool {
	return p.MAC == "" && p.Vlan == nil && p.SpoofChk == "" && p.Trust == "" &&
		p.MinTxRate == nil && p.MaxTxRate == nil && p.Bridge == ""
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
	if p.SpoofChk != "" {
		set = append(set, "spoofchk")
	}
	if p.Trust != "" {
		set = append(set, "trust")
	}
	if p.MinTxRate != nil {
		set = append(set, "minTxRate")
	}
	if p.MaxTxRate != nil {
		set = append(set, "maxTxRate")
	}
	if p.Bridge != "" {
		set = append(set, "bridge")
	}
	return set
}

// ParseClaimParams extracts and validates this driver's opaque parameters
// for one request of an allocated claim. A claim with no matching config
// entry yields empty parameters, which is valid (passthrough devices carry
// none).
func ParseClaimParams(claim *resourceapi.ResourceClaim, driverName, requestName string) (*ClaimParams, error) {
	params := &ClaimParams{}
	if claim.Status.Allocation == nil {
		return params, nil
	}
	for _, config := range claim.Status.Allocation.Devices.Config {
		if config.Opaque == nil ||
			config.Opaque.Driver != driverName ||
			len(config.Requests) > 0 && !slices.Contains(config.Requests, requestName) {
			continue
		}
		if len(config.Opaque.Parameters.Raw) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(config.Opaque.Parameters.Raw))
		decoder.DisallowUnknownFields()
		parsed := &ClaimParams{}
		if err := decoder.Decode(parsed); err != nil {
			return nil, fmt.Errorf("failed to parse opaque parameters for request %s: %w", requestName, err)
		}
		if err := parsed.validate(); err != nil {
			return nil, fmt.Errorf("invalid opaque parameters for request %s: %w", requestName, err)
		}
		return parsed, nil
	}
	return params, nil
}

func (p *ClaimParams) validate() error {
	if p.MAC != "" {
		if err := validateMAC(p.MAC); err != nil {
			return err
		}
	}
	if p.Vlan != nil {
		// 4095 is reserved and 0 means "no VLAN" (IEEE 802.1Q).
		if *p.Vlan < 0 || *p.Vlan > 4094 {
			return fmt.Errorf("VLAN ID must be 0-4094, got %d", *p.Vlan)
		}
	}
	if p.SpoofChk != "" && p.SpoofChk != "on" && p.SpoofChk != "off" {
		return fmt.Errorf("spoofchk must be \"on\" or \"off\", got %q", p.SpoofChk)
	}
	if p.Trust != "" && p.Trust != "on" && p.Trust != "off" {
		return fmt.Errorf("trust must be \"on\" or \"off\", got %q", p.Trust)
	}
	if p.MinTxRate != nil && *p.MinTxRate < 0 {
		return fmt.Errorf("minTxRate must be >= 0, got %d", *p.MinTxRate)
	}
	if p.MaxTxRate != nil && *p.MaxTxRate < 0 {
		return fmt.Errorf("maxTxRate must be >= 0, got %d", *p.MaxTxRate)
	}
	// Mirror the kernel's own rule from rtnl_set_vf_rate():
	//   if (max_tx_rate && max_tx_rate < min_tx_rate) return -EINVAL;
	// A max rate of 0 means "no cap", so it does not constrain the
	// minimum and must not be rejected here.
	if p.MinTxRate != nil && p.MaxTxRate != nil && *p.MaxTxRate > 0 && *p.MinTxRate > *p.MaxTxRate {
		return fmt.Errorf("minTxRate (%d) must be <= maxTxRate (%d)", *p.MinTxRate, *p.MaxTxRate)
	}
	if p.Bridge != "" {
		if err := validateIfName(p.Bridge); err != nil {
			return fmt.Errorf("invalid bridge name: %w", err)
		}
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

// validateIfName applies the kernel's constraints on a network interface
// name (dev_valid_name in net/core/dev.c). IFNAMSIZ caps names at 16 bytes
// including the terminating NUL.
func validateIfName(name string) error {
	const maxIfNameLen = 15
	if len(name) > maxIfNameLen {
		return fmt.Errorf("%q exceeds maximum length of %d characters", name, maxIfNameLen)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%q is not a valid interface name", name)
	}
	for _, r := range name {
		switch r {
		case '/', ' ', '\t', '\n', '\v', '\f', '\r', 0:
			return fmt.Errorf("%q contains characters that are invalid in an interface name", name)
		}
	}
	return nil
}
