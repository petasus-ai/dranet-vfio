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
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	telPF     = "0000:01:00.1"
	telPFName = "enp1s0f1np1"
	telVF     = "0000:01:08.2"
	telVF2    = "0000:01:08.3"
	telClaim  = "6f1c0b9e-2c55-4f7c-9d1e-0c8a4d3b2a10"
)

var telStart = time.Unix(1762675200, 0)

type fakeEthtool struct {
	links map[string]EthtoolLink
	errs  map[string]error
}

func (f *fakeEthtool) GetLink(ifName string) (EthtoolLink, error) {
	return f.links[ifName], f.errs[ifName]
}

type telemetryFixture struct {
	sysfs   *fakeSysfs
	netlink *fakeNetlink
	ethtool *fakeEthtool
	specs   []Spec
	holders map[string][]Holder
	now     time.Time
	tel     *Telemetry
}

// newTelemetryFixture is one mlx5 PF in switchdev mode linked at 25G, with
// VF 2 (held by telClaim) and VF 3 (unheld) published; its VFs 0 and 1 are
// not.
func newTelemetryFixture() *telemetryFixture {
	f := &telemetryFixture{
		sysfs:   &fakeSysfs{driver: map[string]string{telPF: mlx5Driver}},
		netlink: newFakeNetlink(),
		ethtool: &fakeEthtool{
			links: map[string]EthtoolLink{telPFName: {
				HasModes: true, SpeedMbps: 25000, Duplex: "full", Autoneg: "on",
				SupportedMaxMbps: 100000, AdvertisedMaxMbps: 25000, HasLink: true, Link: true,
			}},
			errs: map[string]error{},
		},
		specs: []Spec{
			{PCIAddress: telVF, PFPCIAddress: telPF, PFName: telPFName, VFIndex: 2, Kind: KindVF, LinkType: LinkTypeEthernet},
			{PCIAddress: telVF2, PFPCIAddress: telPF, PFName: telPFName, VFIndex: 3, Kind: KindVF, LinkType: LinkTypeEthernet},
			// Neither a whole PF nor a VF of a PF without a netdev is read.
			{PCIAddress: "0000:51:00.0", Kind: KindPF, VFIndex: -1, LinkType: LinkTypeEthernet},
			{PCIAddress: "0000:61:00.2", PFPCIAddress: "0000:61:00.0", VFIndex: 0, Kind: KindVF, LinkType: LinkTypeEthernet},
		},
		holders: map[string][]Holder{telVF: {{ClaimUID: telClaim, Namespace: "vm-ns", Name: "vm-nic"}}},
		now:     telStart,
	}
	f.netlink.eswitchMode[telPF] = "switchdev"
	f.netlink.vfStats[telPFName] = &PfVfStats{
		OperState: netlink.OperUp,
		VFs:       map[int]VfStats{0: {}, 1: {}, 2: {MaxTxRate: 1000}, 3: {}},
	}
	f.tel = f.telemetry(f.netlink)
	return f
}

// telemetry is a collector over the fixture reading the PF through nlops.
func (f *telemetryFixture) telemetry(nlops NetlinkOps) *Telemetry {
	return NewTelemetry(TelemetryConfig{
		Specs:   func() []Spec { return f.specs },
		Holders: func() map[string][]Holder { return f.holders },
		Sysfs:   f.sysfs,
		Netlink: nlops,
		Ethtool: f.ethtool,
		Now:     func() time.Time { return f.now },
	})
}

// set makes the PF report c as the raw counters of VF index.
func (f *telemetryFixture) set(index int, c vfCounters) {
	vfs := f.netlink.vfStats[telPFName].VFs
	vf := vfs[index]
	vf.RxBytes, vf.TxBytes, vf.RxPackets, vf.TxPackets = c[vfRxBytes], c[vfTxBytes], c[vfRxPackets], c[vfTxPackets]
	vf.Multicast, vf.Broadcast, vf.RxDropped, vf.TxDropped = c[vfRxMulticast], c[vfRxBroadcast], c[vfRxDropped], c[vfTxDropped]
	vfs[index] = vf
}

// at moves the clock to telStart+seconds.
func (f *telemetryFixture) at(seconds int) {
	f.now = telStart.Add(time.Duration(seconds) * time.Second)
}

func (f *telemetryFixture) spec(pci string) Spec {
	for _, s := range f.specs {
		if s.PCIAddress == pci {
			return s
		}
	}
	panic("no spec for " + pci)
}

// series names one series as name{label=value,...}, sorted, without the
// empty labels Prometheus drops.
func series(name string, labels map[string]string) string {
	var pairs []string
	for k, v := range labels {
		if v != "" {
			pairs = append(pairs, k+"="+v)
		}
	}
	sort.Strings(pairs)
	return name + "{" + strings.Join(pairs, ",") + "}"
}

// scrape gathers c through a pedantic registry, by series.
func scrape(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	out, err := gather(c)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return out
}

func gather(c prometheus.Collector) (map[string]float64, error) {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		return nil, err
	}
	mfs, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			value := m.GetGauge().GetValue()
			if m.GetCounter() != nil {
				value = m.GetCounter().GetValue()
			}
			out[series(mf.GetName(), labels)] = value
		}
	}
	return out, nil
}

// vfSeries names a VF series of the fixture's PF.
func vfSeries(name, pci string, index int, h Holder, since time.Time, extra ...string) string {
	labels := map[string]string{
		"pf_pci_address": telPF, "pf_name": telPFName, "vf_index": strconv.Itoa(index), "pci_address": pci,
		"claim_uid": h.ClaimUID, "claim_namespace": h.Namespace, "claim_name": h.Name,
		"since": strconv.FormatInt(since.Unix(), 10),
	}
	for i := 0; i+1 < len(extra); i += 2 {
		labels[extra[i]] = extra[i+1]
	}
	return series(name, labels)
}

func pfSeries(name string, extra ...string) string {
	labels := map[string]string{"pf_pci_address": telPF, "pf_name": telPFName}
	for i := 0; i+1 < len(extra); i += 2 {
		labels[extra[i]] = extra[i+1]
	}
	return series(name, labels)
}

// withPrefix keeps the series whose name starts with prefix.
func withPrefix(all map[string]float64, prefix string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range all {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}

func expectSeries(t *testing.T, got map[string]float64, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		gv, ok := got[k]
		switch {
		case !ok:
			t.Errorf("missing series %s", k)
		case gv != v:
			t.Errorf("%s = %v, want %v", k, gv, v)
		}
	}
}

func expectAbsent(t *testing.T, got map[string]float64, prefixes ...string) {
	t.Helper()
	for k := range got {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				t.Errorf("unexpected series %s", k)
			}
		}
	}
}

// The whole exposition of a switchdev mlx5 PF: counters count from the
// first reading of each VF (the driver start), the held VF carries its
// claim, and drops are neither exported nor claimed.
func TestTelemetryExposition(t *testing.T) {
	f := newTelemetryFixture()
	f.set(2, vfCounters{5000, 7000, 50, 70, 5, 6, 1, 2})
	scrape(t, f.tel)

	f.at(30)
	f.set(2, vfCounters{125000, 67000, 150, 120, 9, 8, 1, 2})
	// Labels in exposition order; both VFs' epochs start at the first scrape.
	claimed := `claim_name="vm-nic",claim_namespace="vm-ns",claim_uid="` + telClaim + `"`
	unclaimed := `claim_name="",claim_namespace="",claim_uid=""`
	vf2 := `pci_address="0000:01:08.2",pf_name="enp1s0f1np1",pf_pci_address="0000:01:00.1",since="1762675200",vf_index="2"`
	vf3 := `pci_address="0000:01:08.3",pf_name="enp1s0f1np1",pf_pci_address="0000:01:00.1",since="1762675200",vf_index="3"`
	held, unheld := claimed+","+vf2, unclaimed+","+vf3
	support := `counters="supported",drops="unsupported"`
	pf := `pf_name="enp1s0f1np1",pf_pci_address="0000:01:00.1"`
	want := `
# HELP dranet_vfio_pf_link_advertised_max_speed_mbps Fastest link mode the PF advertises, in Mb/s.
# TYPE dranet_vfio_pf_link_advertised_max_speed_mbps gauge
dranet_vfio_pf_link_advertised_max_speed_mbps{` + pf + `} 25000
# HELP dranet_vfio_pf_link_info The PF's duplex, autonegotiation, eswitch mode and bound driver.
# TYPE dranet_vfio_pf_link_info gauge
dranet_vfio_pf_link_info{autoneg="on",driver="mlx5_core",duplex="full",eswitch_mode="switchdev",` + pf + `} 1
# HELP dranet_vfio_pf_link_speed_mbps Speed of the PF's link in Mb/s.
# TYPE dranet_vfio_pf_link_speed_mbps gauge
dranet_vfio_pf_link_speed_mbps{` + pf + `} 25000
# HELP dranet_vfio_pf_link_supported_max_speed_mbps Fastest link mode the PF supports, in Mb/s.
# TYPE dranet_vfio_pf_link_supported_max_speed_mbps gauge
dranet_vfio_pf_link_supported_max_speed_mbps{` + pf + `} 100000
# HELP dranet_vfio_pf_link_up 1 when the PF's link is detected.
# TYPE dranet_vfio_pf_link_up gauge
dranet_vfio_pf_link_up{` + pf + `} 1
# HELP dranet_vfio_pf_stats_up 1 when the PF's VF statistics were read over rtnetlink.
# TYPE dranet_vfio_pf_stats_up gauge
dranet_vfio_pf_stats_up{` + pf + `} 1
# HELP dranet_vfio_vf_max_tx_rate_mbps Transmit cap applied to the VF in Mbit/s, 0 = none.
# TYPE dranet_vfio_vf_max_tx_rate_mbps gauge
dranet_vfio_vf_max_tx_rate_mbps{` + held + `} 1000
dranet_vfio_vf_max_tx_rate_mbps{` + unheld + `} 0
# HELP dranet_vfio_vf_rx_broadcast_total Broadcast packets the VF received since the epoch start (since).
# TYPE dranet_vfio_vf_rx_broadcast_total counter
dranet_vfio_vf_rx_broadcast_total{` + held + `} 2
dranet_vfio_vf_rx_broadcast_total{` + unheld + `} 0
# HELP dranet_vfio_vf_rx_bytes_total Bytes the VF received since the epoch start (since).
# TYPE dranet_vfio_vf_rx_bytes_total counter
dranet_vfio_vf_rx_bytes_total{` + held + `} 120000
dranet_vfio_vf_rx_bytes_total{` + unheld + `} 0
# HELP dranet_vfio_vf_rx_multicast_total Multicast packets the VF received since the epoch start (since).
# TYPE dranet_vfio_vf_rx_multicast_total counter
dranet_vfio_vf_rx_multicast_total{` + held + `} 4
dranet_vfio_vf_rx_multicast_total{` + unheld + `} 0
# HELP dranet_vfio_vf_rx_packets_total Packets the VF received since the epoch start (since).
# TYPE dranet_vfio_vf_rx_packets_total counter
dranet_vfio_vf_rx_packets_total{` + held + `} 100
dranet_vfio_vf_rx_packets_total{` + unheld + `} 0
# HELP dranet_vfio_vf_stats_info The VF counters its PF reports: counters supported, unverified (a PF driver not known to fill them) or unsupported (InfiniBand); drops supported in eswitch legacy mode only.
# TYPE dranet_vfio_vf_stats_info gauge
dranet_vfio_vf_stats_info{` + claimed + "," + support + "," + vf2 + `} 1
dranet_vfio_vf_stats_info{` + unclaimed + "," + support + "," + vf3 + `} 1
# HELP dranet_vfio_vf_tx_bytes_total Bytes the VF transmitted since the epoch start (since).
# TYPE dranet_vfio_vf_tx_bytes_total counter
dranet_vfio_vf_tx_bytes_total{` + held + `} 60000
dranet_vfio_vf_tx_bytes_total{` + unheld + `} 0
# HELP dranet_vfio_vf_tx_packets_total Packets the VF transmitted since the epoch start (since).
# TYPE dranet_vfio_vf_tx_packets_total counter
dranet_vfio_vf_tx_packets_total{` + held + `} 50
dranet_vfio_vf_tx_packets_total{` + unheld + `} 0
`
	if err := testutil.CollectAndCompare(f.tel, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

// A prepare starts a new epoch based at the VF's counters then; a counter
// going backwards starts another one. Each epoch is its own series.
func TestTelemetryEpochs(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	rxBytes := func(h Holder, since time.Time) string {
		return vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, h, since)
	}

	f.set(2, vfCounters{1000})
	scrape(t, f.tel)
	f.at(30)
	f.set(2, vfCounters{1100})
	expectSeries(t, scrape(t, f.tel), map[string]float64{rxBytes(a, telStart): 100})

	// Claim b is prepared on the VF at +40 and stored.
	b := Holder{ClaimUID: "claim-b", Namespace: "vm-ns", Name: "vm-nic-2"}
	f.at(40)
	f.set(2, vfCounters{1150})
	f.holders[telVF] = []Holder{b}
	f.tel.StartClaim(f.spec(telVF), b.ClaimUID)
	f.at(60)
	f.set(2, vfCounters{1400})
	got := withPrefix(scrape(t, f.tel), "dranet_vfio_vf_rx_bytes_total")
	want := map[string]float64{
		rxBytes(b, telStart.Add(40*time.Second)):                                 250,
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF2, 3, Holder{}, telStart): 0,
	}
	if len(got) != len(want) {
		t.Fatalf("rx_bytes series = %v, want %v", got, want)
	}
	expectSeries(t, got, want)

	// The VF was re-created: its counters restart below the last reading.
	f.at(90)
	f.set(2, vfCounters{30})
	expectSeries(t, scrape(t, f.tel), map[string]float64{rxBytes(b, telStart.Add(90*time.Second)): 0})
	f.at(120)
	f.set(2, vfCounters{80})
	got = scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{rxBytes(b, telStart.Add(90*time.Second)): 50})
	if _, ok := got[rxBytes(b, telStart.Add(40*time.Second))]; ok {
		t.Error("the epoch before the reset is still exported")
	}
}

// Two resets within one second still give two epochs.
func TestTelemetryEpochsStayDistinct(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	f.set(2, vfCounters{1000})
	scrape(t, f.tel)
	f.set(2, vfCounters{10})
	got := scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, a, telStart.Add(time.Second)): 0,
	})
}

func TestTelemetryDropsInLegacyMode(t *testing.T) {
	f := newTelemetryFixture()
	f.netlink.eswitchMode[telPF] = "legacy"
	a := f.holders[telVF][0]
	f.set(2, vfCounters{0, 0, 0, 0, 0, 0, 10, 20})
	scrape(t, f.tel)
	f.at(30)
	f.set(2, vfCounters{0, 0, 0, 0, 0, 0, 13, 27})
	got := scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_rx_dropped_total", telVF, 2, a, telStart):                                                      3,
		vfSeries("dranet_vfio_vf_tx_dropped_total", telVF, 2, a, telStart):                                                      7,
		vfSeries("dranet_vfio_vf_stats_info", telVF, 2, a, telStart, "counters", "supported", "drops", "supported"):             1,
		pfSeries("dranet_vfio_pf_link_info", "duplex", "full", "autoneg", "on", "eswitch_mode", "legacy", "driver", mlx5Driver): 1,
	})
}

func TestTelemetryNoDropsInSwitchdevMode(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	f.set(2, vfCounters{0, 0, 0, 0, 0, 0, 10, 20})
	got := scrape(t, f.tel)
	expectAbsent(t, got, "dranet_vfio_vf_rx_dropped_total", "dranet_vfio_vf_tx_dropped_total")
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_stats_info", telVF, 2, a, telStart, "counters", "supported", "drops", "unsupported"): 1,
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, a, telStart):                                              0,
	})
}

// rtnetlink reports zeros for a PF driver that fills no VF counters, so
// only mlx5's are exported.
func TestTelemetryUnverifiedDriver(t *testing.T) {
	for name, setup := range map[string]func(f *telemetryFixture){
		"other driver":   func(f *telemetryFixture) { f.sysfs.driver[telPF] = "ice" },
		"unknown driver": func(f *telemetryFixture) { delete(f.sysfs.driver, telPF) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newTelemetryFixture()
			setup(f)
			f.netlink.eswitchMode[telPF] = "legacy"
			a := f.holders[telVF][0]
			got := scrape(t, f.tel)
			expectSeries(t, got, map[string]float64{
				vfSeries("dranet_vfio_vf_stats_info", telVF, 2, a, telStart, "counters", "unverified", "drops", "unsupported"): 1,
				vfSeries("dranet_vfio_vf_max_tx_rate_mbps", telVF, 2, a, telStart):                                             1000,
			})
			expectAbsent(t, got, "dranet_vfio_vf_rx_", "dranet_vfio_vf_tx_")
		})
	}
}

func TestTelemetryInfiniband(t *testing.T) {
	f := newTelemetryFixture()
	for i := range f.specs {
		f.specs[i].LinkType = LinkTypeInfiniband
	}
	a := f.holders[telVF][0]
	got := scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_stats_info", telVF, 2, a, telStart, "counters", "unsupported", "drops", "unsupported"): 1,
		vfSeries("dranet_vfio_vf_max_tx_rate_mbps", telVF, 2, a, telStart):                                              1000,
	})
	expectAbsent(t, got, "dranet_vfio_vf_rx_", "dranet_vfio_vf_tx_")
}

// A PF whose VF statistics cannot be read exports no VF series; its link
// comes from ethtool alone.
func TestTelemetryPFReadFailure(t *testing.T) {
	for name, setup := range map[string]func(f *telemetryFixture){
		"read fails": func(f *telemetryFixture) { delete(f.netlink.vfStats, telPFName) },
		"no VF info": func(f *telemetryFixture) { f.netlink.vfStats[telPFName].VFs = map[int]VfStats{} },
		"no ethtool": func(f *telemetryFixture) {
			delete(f.netlink.vfStats, telPFName)
			f.ethtool.links, f.ethtool.errs[telPFName] = nil, errors.New("ENODEV")
		},
		"no eswitch": func(f *telemetryFixture) {
			delete(f.netlink.vfStats, telPFName)
			delete(f.netlink.eswitchMode, telPF)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTelemetryFixture()
			setup(f)
			got := scrape(t, f.tel)
			expectSeries(t, got, map[string]float64{pfSeries("dranet_vfio_pf_stats_up"): 0})
			expectAbsent(t, got, "dranet_vfio_vf_")
			if _, ok := got[pfSeries("dranet_vfio_pf_link_up")]; ok != (name != "no ethtool") {
				t.Errorf("pf_link_up present = %v", ok)
			}
		})
	}
	f := newTelemetryFixture()
	delete(f.netlink.vfStats, telPFName)
	delete(f.netlink.eswitchMode, telPF)
	f.ethtool.errs[telPFName] = errors.New("ENODEV")
	f.ethtool.links = nil
	got := scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		pfSeries("dranet_vfio_pf_link_info", "duplex", "unknown", "driver", mlx5Driver): 1,
	})
}

// A link without a known speed exports no speed series; without ethtool
// the link state is rtnetlink's carrier.
func TestTelemetryUnknownSpeed(t *testing.T) {
	f := newTelemetryFixture()
	f.ethtool.links[telPFName] = EthtoolLink{HasModes: true, Duplex: "unknown", Autoneg: "on", HasLink: true}
	got := scrape(t, f.tel)
	expectAbsent(t, got, "dranet_vfio_pf_link_speed_mbps", "dranet_vfio_pf_link_supported_max_speed_mbps", "dranet_vfio_pf_link_advertised_max_speed_mbps")
	expectSeries(t, got, map[string]float64{
		pfSeries("dranet_vfio_pf_link_up"): 0,
		pfSeries("dranet_vfio_pf_link_info", "duplex", "unknown", "autoneg", "on", "eswitch_mode", "switchdev", "driver", mlx5Driver): 1,
	})

	for name, tc := range map[string]struct {
		operState uint8
		flags     uint32
		up        float64
	}{
		"operstate up": {netlink.OperUp, 0, 1},
		"lower up":     {netlink.OperUnknown, unix.IFF_LOWER_UP, 1},
		"no carrier":   {netlink.OperDown, unix.IFF_UP, 0},
	} {
		f := newTelemetryFixture()
		f.ethtool.links = nil
		f.ethtool.errs[telPFName] = errors.New("ethtool family not available")
		stats := f.netlink.vfStats[telPFName]
		stats.OperState, stats.Flags = tc.operState, tc.flags
		got := scrape(t, f.tel)
		expectSeries(t, got, map[string]float64{
			pfSeries("dranet_vfio_pf_link_up"):  tc.up,
			pfSeries("dranet_vfio_pf_stats_up"): 1,
		})
		expectAbsent(t, got, "dranet_vfio_pf_link_speed_mbps")
		if t.Failed() {
			t.Fatalf("case %s", name)
		}
	}
}

// A VF two stored entries name has no holder; once one entry goes, the
// other's epoch from its prepare carries on.
func TestTelemetryDuplicateEntries(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	b := Holder{ClaimUID: "claim-b", Namespace: "vm-ns", Name: "vm-nic-2"}
	f.set(2, vfCounters{1000})
	scrape(t, f.tel)

	f.at(10)
	f.set(2, vfCounters{1200})
	f.holders[telVF] = []Holder{a, b}
	f.tel.StartClaim(f.spec(telVF), b.ClaimUID)
	f.at(20)
	f.set(2, vfCounters{1500})
	got := scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, Holder{}, telStart.Add(20*time.Second)): 0,
	})
	if n := len(withPrefix(got, "dranet_vfio_vf_stats_info")); n != 2 {
		t.Errorf("stats_info series = %d, want one per VF", n)
	}

	// a's entry is unprepared and goes.
	f.tel.EndClaim(a.ClaimUID)
	f.holders[telVF] = []Holder{b}
	f.at(30)
	f.set(2, vfCounters{1700})
	got = scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, b, telStart.Add(10*time.Second)): 500,
	})
	if n := len(withPrefix(got, "dranet_vfio_vf_rx_bytes_total")); n != 2 {
		t.Errorf("rx_bytes series = %d, want one per VF", n)
	}
}

// An unprepared claim stops holding its VF at once, while its entry is
// still stored; a later prepare of the same claim starts afresh.
func TestTelemetryEndClaim(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	f.set(2, vfCounters{1000})
	scrape(t, f.tel)

	f.at(10)
	f.tel.EndClaim(a.ClaimUID)
	f.set(2, vfCounters{1100})
	unheld := vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, Holder{}, telStart.Add(10*time.Second))
	expectSeries(t, scrape(t, f.tel), map[string]float64{unheld: 0})

	f.at(20)
	delete(f.holders, telVF)
	f.set(2, vfCounters{1300})
	expectSeries(t, scrape(t, f.tel), map[string]float64{unheld: 200})

	f.at(30)
	f.holders[telVF] = []Holder{a}
	f.tel.StartClaim(f.spec(telVF), a.ClaimUID)
	f.at(40)
	f.set(2, vfCounters{1350})
	got := scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, a, telStart.Add(30*time.Second)): 50,
	})
	if _, ok := got[unheld]; ok {
		t.Error("the unheld epoch is still exported while the VF is held")
	}
}

// A VF the inventory stops publishing loses its epoch.
func TestTelemetryForgetsUnpublishedVFs(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	f.set(2, vfCounters{1000})
	scrape(t, f.tel)

	all := f.specs
	f.specs = all[1:]
	f.at(10)
	if got := scrape(t, f.tel); len(withPrefix(got, "dranet_vfio_vf_rx_bytes_total")) != 1 {
		t.Errorf("rx_bytes series = %v, want the other VF's only", withPrefix(got, "dranet_vfio_vf_rx_bytes_total"))
	}
	f.specs = all
	f.at(20)
	f.set(2, vfCounters{1500})
	expectSeries(t, scrape(t, f.tel), map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, a, telStart.Add(20*time.Second)): 0,
	})
}

// Without a reading at prepare the first scrape starts the epoch.
func TestTelemetryStartClaimWithoutReading(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	pfStats := f.netlink.vfStats[telPFName]
	delete(f.netlink.vfStats, telPFName)
	f.tel.StartClaim(f.spec(telVF), a.ClaimUID)
	f.netlink.vfStats[telPFName] = pfStats
	f.at(15)
	f.set(2, vfCounters{1000})
	expectSeries(t, scrape(t, f.tel), map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, a, telStart.Add(15*time.Second)): 0,
	})
}

// gatedNetlink holds the first LinkGetVfStats after arming until release,
// its reading already taken.
type gatedNetlink struct {
	*fakeNetlink
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func newGatedNetlink(f *fakeNetlink) *gatedNetlink {
	return &gatedNetlink{fakeNetlink: f, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedNetlink) LinkGetVfStats(pfName string) (*PfVfStats, error) {
	g.mu.Lock()
	stats, err := g.fakeNetlink.LinkGetVfStats(pfName)
	if stats != nil {
		c := *stats
		c.VFs = maps.Clone(stats.VFs)
		stats = &c
	}
	hold := g.armed
	g.armed = false
	g.mu.Unlock()
	if hold {
		close(g.entered)
		<-g.release
	}
	return stats, err
}

func (g *gatedNetlink) locked(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn()
}

// A scrape's host reads hold no lock: an unprepare and a prepare during them
// finish, and the scrape commits nothing of the VF it read before them.
func TestTelemetryHooksDoNotWaitForHostReads(t *testing.T) {
	f := newTelemetryFixture()
	gated := newGatedNetlink(f.netlink)
	tel := f.telemetry(gated)
	a := f.holders[telVF][0]
	b := Holder{ClaimUID: "claim-b", Namespace: "vm-ns", Name: "vm-nic-2"}
	f.set(2, vfCounters{1000})
	scrape(t, tel)

	gated.locked(func() { gated.armed = true })
	scraped := make(chan map[string]float64, 1)
	go func() {
		got, err := gather(tel)
		if err != nil {
			t.Errorf("gather: %v", err)
		}
		scraped <- got
	}()
	<-gated.entered
	f.at(10)
	f.holders[telVF] = []Holder{b}
	gated.locked(func() { f.set(2, vfCounters{1200}) })
	hooks := make(chan struct{})
	go func() {
		tel.EndClaim(a.ClaimUID)
		tel.StartClaim(f.spec(telVF), b.ClaimUID)
		close(hooks)
	}()
	select {
	case <-hooks:
	case <-time.After(5 * time.Second):
		close(gated.release)
		<-scraped
		<-hooks
		t.Fatal("prepare and unprepare waited for a scrape's host reads")
	}
	close(gated.release)
	got := <-scraped
	for k := range got {
		if strings.Contains(k, "pci_address="+telVF) {
			t.Errorf("the scrape begun before the prepare exported %s", k)
		}
	}
	expectSeries(t, got, map[string]float64{vfSeries("dranet_vfio_vf_rx_bytes_total", telVF2, 3, Holder{}, telStart): 0})

	f.at(20)
	gated.locked(func() { f.set(2, vfCounters{1500}) })
	expectSeries(t, scrape(t, tel), map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, b, telStart.Add(10*time.Second)): 300,
	})
}

// A counter reset seen while two entries name the VF restarts the epochs
// kept for the stored claims too, not only the exported one.
func TestTelemetryResetRestartsRetainedEpochs(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	b := Holder{ClaimUID: "claim-b", Namespace: "vm-ns", Name: "vm-nic-2"}
	f.set(2, vfCounters{1000})
	scrape(t, f.tel)

	f.at(10)
	f.holders[telVF] = []Holder{a, b}
	f.set(2, vfCounters{1200})
	f.tel.StartClaim(f.spec(telVF), b.ClaimUID)
	f.at(20)
	f.set(2, vfCounters{1500})
	scrape(t, f.tel)

	// The VF is re-created and counts past b's old baseline before a's entry
	// goes.
	f.at(30)
	f.set(2, vfCounters{50})
	expectSeries(t, scrape(t, f.tel), map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, Holder{}, telStart.Add(30*time.Second)): 0,
	})
	f.at(40)
	f.set(2, vfCounters{5000})
	scrape(t, f.tel)

	f.at(50)
	f.tel.EndClaim(a.ClaimUID)
	f.holders[telVF] = []Holder{b}
	f.set(2, vfCounters{5100})
	got := scrape(t, f.tel)
	expectSeries(t, got, map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, b, telStart.Add(30*time.Second)): 5050,
	})
	if _, ok := got[vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, b, telStart.Add(10*time.Second))]; ok {
		t.Error("b's epoch from before the reset resumed")
	}
}

// Every new epoch of a VF starts after the previous one, also when a
// prepare could not read the counters or follows an unprepare in the same
// second.
func TestTelemetrySinceNeverRepeats(t *testing.T) {
	rxBytes := func(h Holder, since time.Time) string {
		return vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, h, since)
	}
	t.Run("prepare without a reading", func(t *testing.T) {
		f := newTelemetryFixture()
		a := f.holders[telVF][0]
		f.set(2, vfCounters{1000})
		scrape(t, f.tel)
		pfStats := f.netlink.vfStats[telPFName]
		delete(f.netlink.vfStats, telPFName)
		f.tel.StartClaim(f.spec(telVF), a.ClaimUID)
		f.netlink.vfStats[telPFName] = pfStats
		f.set(2, vfCounters{1100})
		got := scrape(t, f.tel)
		expectSeries(t, got, map[string]float64{rxBytes(a, telStart.Add(time.Second)): 0})
		if _, ok := got[rxBytes(a, telStart)]; ok {
			t.Error("the new epoch reuses the old one's since")
		}
	})
	t.Run("unprepare and prepare", func(t *testing.T) {
		f := newTelemetryFixture()
		a := f.holders[telVF][0]
		f.set(2, vfCounters{1000})
		scrape(t, f.tel)
		f.tel.EndClaim(a.ClaimUID)
		f.set(2, vfCounters{1100})
		f.tel.StartClaim(f.spec(telVF), a.ClaimUID)
		f.set(2, vfCounters{1150})
		got := scrape(t, f.tel)
		expectSeries(t, got, map[string]float64{rxBytes(a, telStart.Add(time.Second)): 50})
		if _, ok := got[rxBytes(a, telStart)]; ok {
			t.Error("the new epoch reuses the old one's since")
		}
	})
}

// Drops need devlink to state legacy mode: an empty mode (the eswitch query
// failed after the device was found) or an unknown one is no mode.
func TestTelemetryUnknownEswitchMode(t *testing.T) {
	for _, mode := range []string{"", "unknown"} {
		f := newTelemetryFixture()
		f.netlink.eswitchMode[telPF] = mode
		a := f.holders[telVF][0]
		f.set(2, vfCounters{0, 0, 0, 0, 0, 0, 10, 20})
		got := scrape(t, f.tel)
		expectAbsent(t, got, "dranet_vfio_vf_rx_dropped_total", "dranet_vfio_vf_tx_dropped_total")
		expectSeries(t, got, map[string]float64{
			vfSeries("dranet_vfio_vf_stats_info", telVF, 2, a, telStart, "counters", "supported", "drops", "unsupported"): 1,
			pfSeries("dranet_vfio_pf_link_info", "duplex", "full", "autoneg", "on", "driver", mlx5Driver):                 1,
		})
		if t.Failed() {
			t.Fatalf("eswitch mode %q", mode)
		}
	}
}

// A VF its PF reports no entry for has no series and no prepare baseline;
// the others keep theirs.
func TestTelemetryOmitsMissingVFs(t *testing.T) {
	f := newTelemetryFixture()
	a := f.holders[telVF][0]
	delete(f.netlink.vfStats[telPFName].VFs, 2)
	got := scrape(t, f.tel)
	for k := range got {
		if strings.Contains(k, "pci_address="+telVF) {
			t.Errorf("series of a VF without an entry: %s", k)
		}
	}
	expectSeries(t, got, map[string]float64{
		pfSeries("dranet_vfio_pf_stats_up"):                                      1,
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF2, 3, Holder{}, telStart): 0,
	})

	f.tel.StartClaim(f.spec(telVF), a.ClaimUID)
	f.at(10)
	f.set(2, vfCounters{1000})
	expectSeries(t, scrape(t, f.tel), map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, a, telStart.Add(10*time.Second)): 0,
	})
}

// A VF a rescan publishes and a claim prepares while a scrape of the older
// inventory reads the host keeps its prepare baseline.
func TestTelemetryCleanupSparesStateAfterSnapshot(t *testing.T) {
	f := newTelemetryFixture()
	gated := newGatedNetlink(f.netlink)
	tel := f.telemetry(gated)
	a := f.holders[telVF][0]
	all := f.specs
	f.specs = all[1:]

	gated.locked(func() { gated.armed = true })
	scraped := make(chan error, 1)
	go func() {
		_, err := gather(tel)
		scraped <- err
	}()
	<-gated.entered
	f.specs = all
	f.at(10)
	gated.locked(func() { f.set(2, vfCounters{1200}) })
	tel.StartClaim(f.spec(telVF), a.ClaimUID)
	close(gated.release)
	if err := <-scraped; err != nil {
		t.Fatalf("gather: %v", err)
	}

	f.at(20)
	gated.locked(func() { f.set(2, vfCounters{1500}) })
	expectSeries(t, scrape(t, tel), map[string]float64{
		vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, a, telStart.Add(10*time.Second)): 300,
	})
}

// A prepare ends the VF's unheld epoch even when no scrape saw the claim:
// after the unprepare the VF counts afresh, not across the claim.
func TestTelemetryPrepareEndsTheUnheldEpoch(t *testing.T) {
	f := newTelemetryFixture()
	delete(f.holders, telVF)
	b := Holder{ClaimUID: "claim-b", Namespace: "vm-ns", Name: "vm-nic-2"}
	f.set(2, vfCounters{1000})
	scrape(t, f.tel)

	f.at(10)
	f.holders[telVF] = []Holder{b}
	f.set(2, vfCounters{1200})
	f.tel.StartClaim(f.spec(telVF), b.ClaimUID)
	f.at(20)
	f.tel.EndClaim(b.ClaimUID)
	delete(f.holders, telVF)
	f.set(2, vfCounters{5100})
	got := scrape(t, f.tel)
	unheld := vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, Holder{}, telStart.Add(20*time.Second))
	expectSeries(t, got, map[string]float64{unheld: 0})
	if _, ok := got[vfSeries("dranet_vfio_vf_rx_bytes_total", telVF, 2, Holder{}, telStart)]; ok {
		t.Error("the unheld epoch from before the prepare resumed")
	}
	f.at(30)
	f.set(2, vfCounters{5200})
	expectSeries(t, scrape(t, f.tel), map[string]float64{unheld: 100})
}
