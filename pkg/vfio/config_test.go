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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "valid",
			content: `
pools:
  - name: sriov-a
    pfNames: [enp8s0f0np0]
    pfNamesByNode:
      node-33: [ens2f0]
  - name: ib-compute
    kind: vf
    linkType: infiniband
    pfNames: [ibp12s0]
  - name: dpdk-pf
    kind: pf
    pciAddresses: ["0000:51:00.*"]
    resourceName: petasus.io/dpdk-uplink
`,
		},
		{
			name:    "duplicate name",
			content: "pools:\n  - {name: a, pfNames: [p0]}\n  - {name: a, pfNames: [p1]}\n",
			wantErr: "duplicate name",
		},
		{
			name:    "no selector",
			content: "pools:\n  - {name: a}\n",
			wantErr: "at least one selector",
		},
		{
			name:    "bad kind",
			content: "pools:\n  - {name: a, kind: sf, pfNames: [p0]}\n",
			wantErr: "unsupported kind",
		},
		{
			name:    "bad linkType",
			content: "pools:\n  - {name: a, linkType: token-ring, pfNames: [p0]}\n",
			wantErr: "unsupported linkType",
		},
		{
			name:    "pf pool with pfNames",
			content: "pools:\n  - {name: a, kind: pf, pfNames: [p0]}\n",
			wantErr: "use pciAddresses",
		},
		{
			name:    "bad glob",
			content: "pools:\n  - {name: a, pciAddresses: [\"0000:[51:00.0\"]}\n",
			wantErr: "invalid pciAddresses glob",
		},
		{
			name:    "bad name",
			content: "pools:\n  - {name: Not_A_Label, pfNames: [p0]}\n",
			wantErr: "DNS-1123",
		},
		{
			name:    "unknown field",
			content: "pools:\n  - {name: a, pfNames: [p0], capacity: 8}\n",
			wantErr: "failed to parse",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.content))
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

func TestPoolMatches(t *testing.T) {
	vfPool := PoolConfig{
		Name:    "sriov-a",
		PFNames: []string{"enp8s0f0np0"},
		PFNamesByNode: map[string][]string{
			"node-33": {"ens2f0"},
		},
	}
	ibPool := PoolConfig{Name: "ib", LinkType: LinkTypeInfiniband, PFNames: []string{"ibp12s0"}}
	pfPool := PoolConfig{Name: "dpdk", Kind: KindPF, PCIAddresses: []string{"0000:51:00.*"}}

	tests := []struct {
		name                              string
		pool                              PoolConfig
		node, kind, linkType, pfName, pci string
		want                              bool
	}{
		{"vf by pf name", vfPool, "node-1", KindVF, LinkTypeEthernet, "enp8s0f0np0", "0000:08:00.2", true},
		{"per-node override wins", vfPool, "node-33", KindVF, LinkTypeEthernet, "ens2f0", "0000:08:00.2", true},
		{"per-node override replaces default", vfPool, "node-33", KindVF, LinkTypeEthernet, "enp8s0f0np0", "0000:08:00.2", false},
		{"wrong pf", vfPool, "node-1", KindVF, LinkTypeEthernet, "enp8s0f1np1", "0000:08:00.2", false},
		{"kind mismatch", vfPool, "node-1", KindPF, LinkTypeEthernet, "", "0000:08:00.0", false},
		{"linkType filter", ibPool, "node-1", KindVF, LinkTypeEthernet, "ibp12s0", "0000:0c:00.2", false},
		{"linkType match", ibPool, "node-1", KindVF, LinkTypeInfiniband, "ibp12s0", "0000:0c:00.2", true},
		{"pf by pci glob", pfPool, "node-1", KindPF, LinkTypeEthernet, "", "0000:51:00.1", true},
		{"pf glob mismatch", pfPool, "node-1", KindPF, LinkTypeEthernet, "", "0000:52:00.1", false},
		{"empty pf name never matches pfNames", vfPool, "node-1", KindVF, LinkTypeEthernet, "", "0000:08:00.2", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pool.Matches(tc.node, tc.kind, tc.linkType, tc.pfName, tc.pci)
			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}
