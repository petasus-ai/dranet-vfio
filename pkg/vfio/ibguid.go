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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// DefaultIbGuidFile is the GUID record sriov-daemon maintains on the host
// (its StateDirectory), mounted read-only at the same path in the driver pod.
const DefaultIbGuidFile = "/var/lib/petasus/ib-guids.json"

// ibGuidRecord mirrors sriov-daemon's record schema (version 1): the
// InfiniBand node/port GUID of every function it handed to vfio-pci, keyed
// by PCI address, as 16 lowercase hex digits.
type ibGuidRecord struct {
	Version int `json:"version"`
	Devices map[string]struct {
		NodeGUID string `json:"nodeGUID"`
		PortGUID string `json:"portGUID"`
	} `json:"devices"`
}

// ibGuidFile reads the record lazily and re-reads it when its mtime moves,
// so a record written after the driver started shows up on the next rescan.
type ibGuidFile struct {
	path string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	record  *ibGuidRecord
}

func newIbGuidFile(path string) *ibGuidFile { return &ibGuidFile{path: path} }

// Lookup returns the recorded node/port GUID of pci ("" when unknown).
func (f *ibGuidFile) Lookup(pci string) (node, port string) {
	if f == nil || f.path == "" {
		return "", ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh()
	if f.record == nil {
		return "", ""
	}
	dev, ok := f.record.Devices[pci]
	if !ok {
		return "", ""
	}
	return normalizeGUID(dev.NodeGUID), normalizeGUID(dev.PortGUID)
}

func (f *ibGuidFile) refresh() {
	st, err := os.Stat(f.path)
	if err != nil {
		if !os.IsNotExist(err) {
			klog.V(4).Infof("vfio inventory: GUID record %s: %v", f.path, err)
		}
		f.record, f.modTime, f.size = nil, time.Time{}, 0
		return
	}
	if f.record != nil && st.ModTime().Equal(f.modTime) && st.Size() == f.size {
		return
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		klog.Warningf("vfio inventory: read GUID record %s: %v", f.path, err)
		return
	}
	rec := &ibGuidRecord{}
	if err := json.Unmarshal(data, rec); err != nil {
		klog.Warningf("vfio inventory: parse GUID record %s: %v", f.path, err)
		return
	}
	f.record, f.modTime, f.size = rec, st.ModTime(), st.Size()
}

// normalizeGUID returns the 16-hex-digit form of a GUID, or "" for an unset
// or malformed value (all zeros = unprogrammed).
func normalizeGUID(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.TrimPrefix(t, "0x")
	t = strings.ReplaceAll(t, ":", "")
	if len(t) != 16 {
		return ""
	}
	b, err := hex.DecodeString(t)
	if err != nil {
		return ""
	}
	for _, c := range b {
		if c != 0 {
			return t
		}
	}
	return ""
}

// guidBytesToString renders a raw 8-byte GUID as the 16-hex-digit form, ""
// when all zeros.
func guidBytesToString(b [8]byte) string {
	return normalizeGUID(hex.EncodeToString(b[:]))
}

// guidResolver resolves the node/port GUID of InfiniBand devices during one
// inventory pass: a VF's administrative GUIDs live on its PF netdev (read
// once per PF), a whole PF's only in the record; the record backs a VF whose
// PF reports nothing. Unknown stays unknown — attributes are omitted, never
// published empty.
type guidResolver struct {
	netlink NetlinkOps
	record  *ibGuidFile
	byPF    map[string]map[int]VfIbGUID
}

func (r *guidResolver) resolve(spec *Spec) (node, port string) {
	if spec.LinkType != LinkTypeInfiniband {
		return "", ""
	}
	if spec.Kind == KindVF && spec.PFName != "" {
		if r.byPF == nil {
			r.byPF = map[string]map[int]VfIbGUID{}
		}
		vfs, ok := r.byPF[spec.PFName]
		if !ok {
			got, err := r.netlink.LinkGetVfIbGUIDs(spec.PFName)
			if err != nil {
				klog.V(4).Infof("vfio inventory: VF GUIDs of PF %s: %v", spec.PFName, err)
			}
			vfs = got
			r.byPF[spec.PFName] = vfs
		}
		if g, ok := vfs[spec.VFIndex]; ok {
			node, port = guidBytesToString(g.NodeGUID), guidBytesToString(g.PortGUID)
			if node != "" && port != "" {
				return node, port
			}
		}
	}
	rn, rp := r.record.Lookup(spec.PCIAddress)
	if node == "" {
		node = rn
	}
	if port == "" {
		port = rp
	}
	return node, port
}

func (r *guidResolver) String() string {
	return fmt.Sprintf("guidResolver{record=%v}", r.record != nil && r.record.path != "")
}
