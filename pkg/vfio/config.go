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
	"errors"
	"fmt"
	"os"
	"path"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	// KindVF and KindPF are the device kinds a pool selects.
	KindVF = "vf"
	KindPF = "pf"

	// LinkTypeEthernet, LinkTypeInfiniband and LinkTypeOther classify a
	// device by PCI class: 0x0200* is Ethernet, 0x0207*/0x0c06* is
	// InfiniBand, every other network class is "other".
	LinkTypeEthernet   = "ethernet"
	LinkTypeInfiniband = "infiniband"
	LinkTypeOther      = "other"
)

// PoolConfig maps discovered vfio-pci-bound network devices into one pool.
// A device belongs to the pool when its kind and link type match and one of
// the selectors matches: the parent PF's netdev name is listed in pfNames,
// the parent PF's PCI address matches a pfPciAddresses glob, or the
// device's own PCI address matches a pciAddresses glob.
type PoolConfig struct {
	// Name identifies the pool. Must be a DNS-1123 label.
	Name string `json:"name"`

	// ResourceName is published as the "k8s.cni.cncf.io/resourceName"
	// attribute. Defaults to "petasus.io/<name>".
	ResourceName string `json:"resourceName,omitempty"`

	// Kind selects VFs ("vf", the default) or whole-PF passthrough ("pf").
	Kind string `json:"kind,omitempty"`

	// LinkType optionally restricts the pool to "ethernet" or "infiniband"
	// devices.
	LinkType string `json:"linkType,omitempty"`

	// PFNames selects VFs by their parent PF's netdev name. Only valid for
	// kind "vf": a PF that is itself vfio-pci-bound has no netdev, so PF
	// pools must select by PCI address.
	PFNames []string `json:"pfNames,omitempty"`

	// PFNamesByNode overrides PFNames for specific node names.
	PFNamesByNode map[string][]string `json:"pfNamesByNode,omitempty"`

	// PFPciAddresses selects VFs whose parent PF's PCI address matches one
	// of these globs (path.Match syntax). Only valid for kind "vf": a PF
	// pool already selects by the device's own address via pciAddresses.
	PFPciAddresses []string `json:"pfPciAddresses,omitempty"`

	// PFPciAddressesByNode overrides PFPciAddresses for specific node names.
	PFPciAddressesByNode map[string][]string `json:"pfPciAddressesByNode,omitempty"`

	// PCIAddresses selects devices whose own PCI address matches one of
	// these globs (path.Match syntax, e.g. "0000:51:00.*").
	PCIAddresses []string `json:"pciAddresses,omitempty"`

	// PCIAddressesByNode overrides PCIAddresses for specific node names.
	PCIAddressesByNode map[string][]string `json:"pciAddressesByNode,omitempty"`
}

// Config is the file format of the driver configuration.
type Config struct {
	Pools []PoolConfig `json:"pools"`
}

// KindOrDefault returns the pool's device kind, defaulting to "vf".
func (p *PoolConfig) KindOrDefault() string {
	if p.Kind == "" {
		return KindVF
	}
	return p.Kind
}

// PFNamesForNode resolves the PF-name selector for the given node.
func (p *PoolConfig) PFNamesForNode(nodeName string) []string {
	if names, ok := p.PFNamesByNode[nodeName]; ok {
		return names
	}
	return p.PFNames
}

// PFPciAddressesForNode resolves the PF-PCI-address selector for the given
// node.
func (p *PoolConfig) PFPciAddressesForNode(nodeName string) []string {
	if addrs, ok := p.PFPciAddressesByNode[nodeName]; ok {
		return addrs
	}
	return p.PFPciAddresses
}

// PCIAddressesForNode resolves the PCI-address selector for the given node.
func (p *PoolConfig) PCIAddressesForNode(nodeName string) []string {
	if addrs, ok := p.PCIAddressesByNode[nodeName]; ok {
		return addrs
	}
	return p.PCIAddresses
}

// Matches reports whether a discovered device belongs to this pool on the
// given node.
func (p *PoolConfig) Matches(nodeName, kind, linkType, pfName, pfPciAddress, pciAddress string) bool {
	if p.KindOrDefault() != kind {
		return false
	}
	if p.LinkType != "" && p.LinkType != linkType {
		return false
	}
	for _, name := range p.PFNamesForNode(nodeName) {
		if pfName != "" && name == pfName {
			return true
		}
	}
	for _, glob := range p.PFPciAddressesForNode(nodeName) {
		if pfPciAddress == "" {
			break
		}
		if ok, err := path.Match(glob, pfPciAddress); err == nil && ok {
			return true
		}
	}
	for _, glob := range p.PCIAddressesForNode(nodeName) {
		if ok, err := path.Match(glob, pciAddress); err == nil && ok {
			return true
		}
	}
	return false
}

// LoadConfig reads and validates the configuration file.
func LoadConfig(configPath string) (*Config, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	config := &Config{}
	if err := yaml.UnmarshalStrict(raw, config); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", configPath, err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}

// Validate checks the whole configuration.
func (c *Config) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for i := range c.Pools {
		pool := &c.Pools[i]
		if seen[pool.Name] {
			errs = append(errs, fmt.Errorf("pool %q: duplicate name", pool.Name))
		}
		seen[pool.Name] = true
		errs = append(errs, pool.validate()...)
	}
	return errors.Join(errs...)
}

func (p *PoolConfig) validate() []error {
	var errs []error
	if msgs := validation.IsDNS1123Label(p.Name); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("pool %q: name must be a DNS-1123 label: %v", p.Name, msgs))
	}
	switch p.Kind {
	case "", KindVF, KindPF:
	default:
		errs = append(errs, fmt.Errorf("pool %q: unsupported kind %q", p.Name, p.Kind))
	}
	switch p.LinkType {
	case "", LinkTypeEthernet, LinkTypeInfiniband:
	default:
		errs = append(errs, fmt.Errorf("pool %q: unsupported linkType %q", p.Name, p.LinkType))
	}
	hasPFNames := len(p.PFNames) > 0 || len(p.PFNamesByNode) > 0
	hasPFPciAddresses := len(p.PFPciAddresses) > 0 || len(p.PFPciAddressesByNode) > 0
	hasPCIAddresses := len(p.PCIAddresses) > 0 || len(p.PCIAddressesByNode) > 0
	if !hasPFNames && !hasPFPciAddresses && !hasPCIAddresses {
		errs = append(errs, fmt.Errorf("pool %q: at least one selector (pfNames, pfPciAddresses or pciAddresses) is required", p.Name))
	}
	if p.KindOrDefault() == KindPF && hasPFNames {
		errs = append(errs, fmt.Errorf("pool %q: pfNames selects VFs by parent PF; a vfio-pci-bound PF has no netdev, use pciAddresses", p.Name))
	}
	if p.KindOrDefault() == KindPF && hasPFPciAddresses {
		errs = append(errs, fmt.Errorf("pool %q: pfPciAddresses selects VFs by parent PF; a PF pool selects its own address via pciAddresses", p.Name))
	}
	globLists := append([][]string{p.PCIAddresses, p.PFPciAddresses}, mapValues(p.PCIAddressesByNode)...)
	globLists = append(globLists, mapValues(p.PFPciAddressesByNode)...)
	for _, globs := range globLists {
		for _, glob := range globs {
			if _, err := path.Match(glob, "0000:00:00.0"); err != nil {
				errs = append(errs, fmt.Errorf("pool %q: invalid pciAddresses glob %q: %v", p.Name, glob, err))
			}
		}
	}
	return errs
}

func mapValues(m map[string][]string) [][]string {
	values := make([][]string, 0, len(m))
	for _, v := range m {
		values = append(values, v)
	}
	return values
}
