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
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// mlx5Driver is the one PF driver with an eswitch mode (devlink); every other
// driver's VFs are legacy.
const mlx5Driver = "mlx5_core"

// vfCounterDrivers fill IFLA_VF_STATS (ndo_get_vf_stats) on every supported
// kernel; rtnetlink reports zeros for any other driver.
var vfCounterDrivers = map[string]bool{mlx5Driver: true, "i40e": true, "ice": true}

// VF counter support, the counters and drops labels of
// dranet_vfio_vf_stats_info.
const (
	statsSupported   = "supported"
	statsUnverified  = "unverified"
	statsUnsupported = "unsupported"
)

// The IFLA_VF_STATS counters, in vfCounterDescs order.
const (
	vfRxBytes = iota
	vfTxBytes
	vfRxPackets
	vfTxPackets
	vfRxMulticast
	vfRxBroadcast
	vfRxDropped
	vfTxDropped
	numVfCounters
)

var (
	pfLabels = []string{"pf_pci_address", "pf_name"}
	vfLabels = []string{"pf_pci_address", "pf_name", "vf_index", "pci_address", "claim_uid", "claim_namespace", "claim_name", "since"}

	vfCounterDescs = [numVfCounters]*prometheus.Desc{
		vfRxBytes:     vfDesc("rx_bytes_total", "Bytes the VF received since the epoch start (since)."),
		vfTxBytes:     vfDesc("tx_bytes_total", "Bytes the VF transmitted since the epoch start (since)."),
		vfRxPackets:   vfDesc("rx_packets_total", "Packets the VF received since the epoch start (since)."),
		vfTxPackets:   vfDesc("tx_packets_total", "Packets the VF transmitted since the epoch start (since)."),
		vfRxMulticast: vfDesc("rx_multicast_total", "Multicast packets the VF received since the epoch start (since)."),
		vfRxBroadcast: vfDesc("rx_broadcast_total", "Broadcast packets the VF received since the epoch start (since)."),
		vfRxDropped:   vfDesc("rx_dropped_total", "Packets dropped on the VF's receive path since the epoch start (since); eswitch legacy mode only."),
		vfTxDropped:   vfDesc("tx_dropped_total", "Packets dropped on the VF's transmit path since the epoch start (since); eswitch legacy mode only."),
	}
	vfStatsInfoDesc = prometheus.NewDesc("dranet_vfio_vf_stats_info",
		"The VF counters its PF reports: counters supported, unverified (a PF driver not known to fill them) or unsupported (InfiniBand); drops supported in eswitch legacy mode only.",
		slices.Concat(vfLabels, []string{"counters", "drops"}), nil)
	vfMaxTxRateDesc = vfDesc("max_tx_rate_mbps", "Transmit cap applied to the VF in Mbit/s, 0 = none.")

	pfStatsUpDesc       = pfDesc("stats_up", "1 when the PF's VF statistics were read over rtnetlink.")
	pfLinkUpDesc        = pfDesc("link_up", "1 when the PF's link is detected.")
	pfLinkSpeedDesc     = pfDesc("link_speed_mbps", "Speed of the PF's link in Mb/s while it is up.")
	pfSupportedMaxDesc  = pfDesc("link_supported_max_speed_mbps", "Fastest link mode the PF supports, in Mb/s.")
	pfAdvertisedMaxDesc = pfDesc("link_advertised_max_speed_mbps", "Fastest link mode the PF advertises, in Mb/s.")
	pfLinkInfoDesc      = prometheus.NewDesc("dranet_vfio_pf_link_info",
		"The PF's duplex, autonegotiation, eswitch mode and bound driver.",
		slices.Concat(pfLabels, []string{"duplex", "autoneg", "eswitch_mode", "driver"}), nil)
)

func vfDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc("dranet_vfio_vf_"+name, help, vfLabels, nil)
}

func pfDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc("dranet_vfio_pf_"+name, help, pfLabels, nil)
}

// Holder is a claim the driver's checkpoint records as prepared on a VF.
type Holder struct {
	ClaimUID  string
	Namespace string
	Name      string
}

// TelemetryConfig wires a Telemetry. The host accessors and the clock
// default to the real ones.
type TelemetryConfig struct {
	// Specs lists the published devices.
	Specs func() []Spec
	// Holders lists the checkpointed claims by VF PCI address.
	Holders func() map[string][]Holder

	Sysfs   SysfsOps
	Netlink NetlinkOps
	Ethtool EthtoolOps
	Now     func() time.Time
}

// Telemetry is the prometheus.Collector of the published VFs and their PFs
// as the host reports them, read at scrape time: per PF one rtnetlink link
// read (VF counters and rates), its ethtool link, devlink eswitch mode and
// bound driver. A PF without a netdev is not read. The host reads run
// outside the lock and are committed under it.
//
// VF counters count from the start of an epoch, the since label (unix
// seconds, increasing per VF). An epoch belongs to one VF and its holder
// (claim UID, "" when unheld or named by several stored entries) and starts
// when a claim is prepared on the VF and when the collector first sees the
// VF; a raw counter going backwards restarts every epoch of the VF. Epochs
// live in memory only.
type Telemetry struct {
	specs   func() []Spec
	holders func() map[string][]Holder
	sysfs   SysfsOps
	netlink NetlinkOps
	ethtool EthtoolOps
	now     func() time.Time

	// mu guards vfs and released.
	mu sync.Mutex
	// vfs by PCI address, kept after a VF stops being published so its
	// generation and since only grow.
	vfs map[string]*vfState
	// released holds the claims ended while their entries may still be
	// stored; such an entry holds no VF.
	released map[string]bool
}

type vfCounters [numVfCounters]uint64

func vfCountersOf(vf VfStats) vfCounters {
	return vfCounters{
		vfRxBytes:     vf.RxBytes,
		vfTxBytes:     vf.TxBytes,
		vfRxPackets:   vf.RxPackets,
		vfTxPackets:   vf.TxPackets,
		vfRxMulticast: vf.Multicast,
		vfRxBroadcast: vf.Broadcast,
		vfRxDropped:   vf.RxDropped,
		vfTxDropped:   vf.TxDropped,
	}
}

// below reports whether any counter is lower than in prev.
func (c vfCounters) below(prev vfCounters) bool {
	for i := range c {
		if c[i] < prev[i] {
			return true
		}
	}
	return false
}

// vfState is what the collector keeps of one VF.
type vfState struct {
	// gen counts the committed changes; a reading begun before one is not
	// committed.
	gen uint64
	// last is the latest committed raw reading.
	last    vfCounters
	hasLast bool
	// highSince is the latest epoch start handed out.
	highSince int64
	// epochs by claim UID.
	epochs map[string]*vfEpoch
}

type vfEpoch struct {
	since int64
	base  vfCounters
}

// nextSince returns the start of a new epoch, after every earlier one.
func (st *vfState) nextSince(now time.Time) int64 {
	st.highSince = max(now.Unix(), st.highSince+1)
	return st.highSince
}

// observe commits a raw reading; a counter below the last reading restarts
// every epoch of the VF there.
func (st *vfState) observe(raw vfCounters, now time.Time) {
	if st.hasLast && raw.below(st.last) {
		since := st.nextSince(now)
		for _, e := range st.epochs {
			e.since, e.base = since, raw
		}
	}
	st.last, st.hasLast = raw, true
	st.gen++
}

// NewTelemetry returns the collector; register it to export it.
func NewTelemetry(cfg TelemetryConfig) *Telemetry {
	t := &Telemetry{
		specs:    cfg.Specs,
		holders:  cfg.Holders,
		sysfs:    cfg.Sysfs,
		netlink:  cfg.Netlink,
		ethtool:  cfg.Ethtool,
		now:      cfg.Now,
		vfs:      map[string]*vfState{},
		released: map[string]bool{},
	}
	if t.specs == nil {
		t.specs = func() []Spec { return nil }
	}
	if t.holders == nil {
		t.holders = func() map[string][]Holder { return nil }
	}
	if t.sysfs == nil {
		t.sysfs = NewSysfs()
	}
	if t.netlink == nil {
		t.netlink = NewNetlink()
	}
	if t.ethtool == nil {
		t.ethtool = NewEthtool()
	}
	if t.now == nil {
		t.now = time.Now
	}
	return t
}

// Describe implements prometheus.Collector.
func (t *Telemetry) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range vfCounterDescs {
		ch <- desc
	}
	for _, desc := range []*prometheus.Desc{vfStatsInfoDesc, vfMaxTxRateDesc, pfStatsUpDesc, pfLinkUpDesc,
		pfLinkSpeedDesc, pfSupportedMaxDesc, pfAdvertisedMaxDesc, pfLinkInfoDesc} {
		ch <- desc
	}
}

// Collect implements prometheus.Collector.
func (t *Telemetry) Collect(ch chan<- prometheus.Metric) {
	pfs := telemetryPFs(t.specs())
	gens := t.generations(pfs)
	readings := make([]pfReading, len(pfs))
	for i, pf := range pfs {
		readings[i] = t.readPF(pf)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	holders := t.holders()
	t.forgetReleased(holders)
	published := map[string]bool{}
	for i, pf := range pfs {
		for _, vf := range pf.vfs {
			published[vf.PCIAddress] = true
		}
		t.commitPF(ch, pf, readings[i], gens, holders)
	}
	for pci, st := range t.vfs {
		// Only state as old as the snapshot: a VF published and prepared
		// during the reads is not this snapshot's to drop.
		if gen, ok := gens[pci]; ok && gen == st.gen && !published[pci] && (st.hasLast || len(st.epochs) > 0) {
			clear(st.epochs)
			st.hasLast = false
			st.gen++
		}
	}
}

// StartClaim starts a new epoch for the VF a claim was just prepared on,
// based at the VF's counters now; without a reading the next scrape starts
// it.
func (t *Telemetry) StartClaim(spec Spec, claimUID string) {
	if t == nil || spec.Kind != KindVF || spec.PFName == "" || claimUID == "" {
		return
	}
	t.mu.Lock()
	gen := t.stateOf(spec.PCIAddress).gen
	t.mu.Unlock()
	raw, err := t.readVf(spec)
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.released, claimUID)
	st := t.stateOf(spec.PCIAddress)
	delete(st.epochs, claimUID)
	// The VF's unheld epoch ends with the prepare, seen by a scrape or not.
	delete(st.epochs, "")
	if err == nil && st.gen != gen {
		err = errors.New("the VF changed during the read")
	}
	if err != nil {
		// A scrape's reading begun before the prepare must not base the
		// claim's epoch either.
		st.gen++
		klog.V(4).Infof("vfio telemetry: no counters of VF %s at prepare: %v", spec.PCIAddress, err)
		return
	}
	st.observe(raw, now)
	st.epochs[claimUID] = &vfEpoch{since: st.nextSince(now), base: raw}
}

// EndClaim drops the epochs of a claim being unprepared: its stored entries
// no longer hold their VFs.
func (t *Telemetry) EndClaim(claimUID string) {
	if t == nil || claimUID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.forgetReleased(t.holders())
	t.released[claimUID] = true
	for _, st := range t.vfs {
		if _, ok := st.epochs[claimUID]; ok {
			delete(st.epochs, claimUID)
			st.gen++
		}
	}
}

// forgetReleased drops the ended claims no entry is stored for any more.
func (t *Telemetry) forgetReleased(holders map[string][]Holder) {
	for uid := range t.released {
		if !storedAnywhere(holders, uid) {
			delete(t.released, uid)
		}
	}
}

// telemetryPF is a PF netdev and its published VFs.
type telemetryPF struct {
	pci  string
	name string
	vfs  []Spec
}

func telemetryPFs(specs []Spec) []*telemetryPF {
	var pfs []*telemetryPF
	byPci := map[string]*telemetryPF{}
	for _, spec := range specs {
		if spec.Kind != KindVF || spec.PFName == "" || spec.PFPCIAddress == "" {
			continue
		}
		pf := byPci[spec.PFPCIAddress]
		if pf == nil {
			pf = &telemetryPF{pci: spec.PFPCIAddress, name: spec.PFName}
			byPci[spec.PFPCIAddress] = pf
			pfs = append(pfs, pf)
		}
		pf.vfs = append(pf.vfs, spec)
	}
	return pfs
}

// generations snapshots the generation of every known and published VF
// before the reads.
func (t *Telemetry) generations(pfs []*telemetryPF) map[string]uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	gens := map[string]uint64{}
	for pci, st := range t.vfs {
		gens[pci] = st.gen
	}
	for _, pf := range pfs {
		for _, vf := range pf.vfs {
			gens[vf.PCIAddress] = t.stateOf(vf.PCIAddress).gen
		}
	}
	return gens
}

// pfReading is what one PF's host reads returned.
type pfReading struct {
	stats  *PfVfStats // nil when the read failed
	at     time.Time
	eth    EthtoolLink
	mode   Mode   // "" when unknown
	driver string // "" when unknown
}

func (t *Telemetry) readPF(pf *telemetryPF) pfReading {
	var r pfReading
	stats, err := t.netlink.LinkGetVfStats(pf.name)
	r.at = t.now()
	if err != nil {
		klog.V(4).Infof("vfio telemetry: PF %s (%s): %v", pf.name, pf.pci, err)
	} else {
		r.stats = stats
	}
	if r.eth, err = t.ethtool.GetLink(pf.name); err != nil {
		klog.V(4).Infof("vfio telemetry: ethtool of PF %s (%s): %v", pf.name, pf.pci, err)
	}
	if r.driver, err = t.sysfs.GetDriver(pf.pci); err != nil {
		klog.V(4).Infof("vfio telemetry: PF %s: %v", pf.pci, err)
	}
	switch r.driver {
	case mlx5Driver:
		r.mode = telemetryEswitchMode(t.netlink, pf.pci)
	case "":
	default:
		r.mode = ModeLegacy
	}
	return r
}

// telemetryEswitchMode is the eswitch mode devlink states for the PF, ""
// otherwise: netlink reports an empty mode when only the eswitch query
// fails.
func telemetryEswitchMode(nlops NetlinkOps, pfPci string) Mode {
	mode, err := nlops.DevLinkGetEswitchMode(pfPci)
	if err != nil {
		klog.V(4).Infof("vfio telemetry: PF %s: %v", pfPci, err)
		return ""
	}
	switch mode {
	case string(ModeSwitchdev):
		return ModeSwitchdev
	case string(ModeLegacy):
		return ModeLegacy
	}
	return ""
}

func (t *Telemetry) commitPF(ch chan<- prometheus.Metric, pf *telemetryPF, r pfReading, gens map[string]uint64, holders map[string][]Holder) {
	// No VF entries at all: missing CAP_NET_ADMIN, or SR-IOV disabled.
	statsUp := r.stats != nil && len(r.stats.VFs) > 0
	gauge(ch, pfStatsUpDesc, boolValue(statsUp), pf.pci, pf.name)
	up := false
	switch {
	case r.eth.HasLink:
		up = r.eth.Link
		gauge(ch, pfLinkUpDesc, boolValue(up), pf.pci, pf.name)
	case r.stats != nil:
		up = carrier(r.stats)
		gauge(ch, pfLinkUpDesc, boolValue(up), pf.pci, pf.name)
	}
	// A driver may keep reporting the last speed of a link that is down.
	if up && r.eth.SpeedMbps > 0 {
		gauge(ch, pfLinkSpeedDesc, float64(r.eth.SpeedMbps), pf.pci, pf.name)
	}
	if r.eth.SupportedMaxMbps > 0 {
		gauge(ch, pfSupportedMaxDesc, float64(r.eth.SupportedMaxMbps), pf.pci, pf.name)
	}
	if r.eth.AdvertisedMaxMbps > 0 {
		gauge(ch, pfAdvertisedMaxDesc, float64(r.eth.AdvertisedMaxMbps), pf.pci, pf.name)
	}
	duplex := r.eth.Duplex
	if duplex == "" {
		duplex = "unknown"
	}
	gauge(ch, pfLinkInfoDesc, 1, pf.pci, pf.name, duplex, r.eth.Autoneg, string(r.mode), r.driver)

	if !statsUp {
		return
	}
	for _, vf := range pf.vfs {
		info, ok := r.stats.VFs[vf.VFIndex]
		if !ok {
			continue
		}
		st := t.stateOf(vf.PCIAddress)
		if st.gen != gens[vf.PCIAddress] {
			// A prepare, an unprepare or another scrape changed the VF
			// after this reading began.
			continue
		}
		raw := vfCountersOf(info)
		st.observe(raw, r.at)
		stored := holders[vf.PCIAddress]
		holder := t.holderOf(stored)
		e := t.epochFor(st, holder.ClaimUID, stored, raw, r.at)
		labels := []string{pf.pci, pf.name, strconv.Itoa(vf.VFIndex), vf.PCIAddress,
			holder.ClaimUID, holder.Namespace, holder.Name, strconv.FormatInt(e.since, 10)}
		counters, drops := vfStatsSupport(vf.LinkType, r.driver, r.mode)
		gauge(ch, vfStatsInfoDesc, 1, slices.Concat(labels, []string{counters, drops})...)
		gauge(ch, vfMaxTxRateDesc, float64(info.MaxTxRate), labels...)
		if counters != statsSupported {
			continue
		}
		for i, desc := range vfCounterDescs {
			if (i == vfRxDropped || i == vfTxDropped) && drops != statsSupported {
				continue
			}
			emit(ch, desc, prometheus.CounterValue, float64(raw[i]-e.base[i]), labels...)
		}
	}
}

// vfStatsSupport tells which counters a VF's PF fills: mlx5 in switchdev mode
// leaves the drop counters at zero, and an InfiniBand VF reports none. Drops
// need a legacy mode.
func vfStatsSupport(linkType, driver string, mode Mode) (counters, drops string) {
	switch {
	case linkType == LinkTypeInfiniband:
		return statsUnsupported, statsUnsupported
	case !vfCounterDrivers[driver]:
		return statsUnverified, statsUnsupported
	case mode == ModeLegacy:
		return statsSupported, statsSupported
	default:
		return statsSupported, statsUnsupported
	}
}

// holderOf returns the one claim stored for a VF, none when no claim or
// several are.
func (t *Telemetry) holderOf(stored []Holder) Holder {
	var live []Holder
	for _, h := range stored {
		if !t.released[h.ClaimUID] {
			live = append(live, h)
		}
	}
	if len(live) == 1 {
		return live[0]
	}
	return Holder{}
}

// epochFor returns the VF's epoch of claimUID, starting it at raw when there
// is none. Every other epoch of the VF ends, except those of claims still
// stored for it.
func (t *Telemetry) epochFor(st *vfState, claimUID string, stored []Holder, raw vfCounters, now time.Time) *vfEpoch {
	e := st.epochs[claimUID]
	if e == nil {
		e = &vfEpoch{since: st.nextSince(now), base: raw}
		st.epochs[claimUID] = e
	}
	for uid := range st.epochs {
		if uid != claimUID && (uid == "" || !t.stores(stored, uid)) {
			delete(st.epochs, uid)
		}
	}
	return e
}

// stores reports whether a live entry of claimUID is stored.
func (t *Telemetry) stores(stored []Holder, claimUID string) bool {
	if t.released[claimUID] {
		return false
	}
	for _, h := range stored {
		if h.ClaimUID == claimUID {
			return true
		}
	}
	return false
}

func storedAnywhere(holders map[string][]Holder, claimUID string) bool {
	for _, stored := range holders {
		for _, h := range stored {
			if h.ClaimUID == claimUID {
				return true
			}
		}
	}
	return false
}

func (t *Telemetry) stateOf(pci string) *vfState {
	st := t.vfs[pci]
	if st == nil {
		st = &vfState{epochs: map[string]*vfEpoch{}}
		t.vfs[pci] = st
	}
	return st
}

// readVf reads one VF's counters from its PF.
func (t *Telemetry) readVf(spec Spec) (vfCounters, error) {
	stats, err := t.netlink.LinkGetVfStats(spec.PFName)
	if err != nil {
		return vfCounters{}, err
	}
	info, ok := stats.VFs[spec.VFIndex]
	if !ok {
		return vfCounters{}, fmt.Errorf("PF %s reports no VF %d", spec.PFName, spec.VFIndex)
	}
	return vfCountersOf(info), nil
}

// carrier is the link state rtnetlink reports.
func carrier(stats *PfVfStats) bool {
	return stats.OperState == netlink.OperUp || stats.Flags&unix.IFF_LOWER_UP != 0
}

func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	emit(ch, desc, prometheus.GaugeValue, value, labels...)
}

// emit sends one series; a malformed one is logged and left out rather than
// failing the whole scrape.
func emit(ch chan<- prometheus.Metric, desc *prometheus.Desc, valueType prometheus.ValueType, value float64, labels ...string) {
	m, err := prometheus.NewConstMetric(desc, valueType, value, labels...)
	if err != nil {
		klog.Errorf("vfio telemetry: %v", err)
		return
	}
	ch <- m
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
