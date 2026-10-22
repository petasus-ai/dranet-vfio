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

package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1"
	drapbv1beta1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/utils/set"
	"sigs.k8s.io/dranet/pkg/vfio"
	"sigs.k8s.io/dranet/pkg/vfio/cdi"
)

// fullCarryDeviceConfig populates every carried field for the checkpoint
// wire-format fixture (fullDeviceConfig).
func fullCarryDeviceConfig() CarryDeviceConfig {
	return CarryDeviceConfig{
		VfioPCIAddress: "0000:01:08.2",
		VfioState: &vfio.HostState{
			Mode:    vfio.ModeLegacy,
			VfPci:   "0000:01:08.2",
			PfPci:   "0000:01:00.0",
			PfName:  "ens1f0",
			VfIndex: 2,
			Vlan:    3001,
			OrigMAC: "02:00:00:00:00:03",
		},
		VfioDevices: []LinuxDevice{{
			Path:     "/dev/vfio/12",
			Type:     "c",
			Major:    509,
			Minor:    1,
			FileMode: 0o660,
			UID:      107,
			GID:      107,
		}},
		CDIDeviceID: "vfio.petasus.io/vfio=uid_nic_pci-0000-01-08-2",
	}
}

func TestWatchHealthStatusHoldsTheStream(t *testing.T) {
	defer func(interval time.Duration) { carryHealthReportInterval = interval }(carryHealthReportInterval)
	carryHealthReportInterval = 20 * time.Millisecond

	np := &NetworkDriver{}
	ctx, cancel := context.WithCancel(context.Background())
	reports := make(chan kubeletplugin.DeviceHealthReport)
	done := make(chan error, 1)
	go func() { done <- np.WatchHealthStatus(ctx, reports) }()

	// The report is re-sent, so the helper never sees a stale stream.
	for i := 0; i < 2; i++ {
		select {
		case <-reports:
		case <-time.After(time.Second):
			t.Fatalf("health report %d not sent", i+1)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("WatchHealthStatus returned %v before the stream was closed", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchHealthStatus returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WatchHealthStatus did not return after the stream was closed")
	}
}

// A checkpointed vfio device the inventory has not scanned yet (driver
// restart) must fail the prepare, not reach the upstream stored-config
// shortcut that would answer without the CDI device.
func TestPrepareCarryDeviceBeforeInventoryScan(t *testing.T) {
	np := &NetworkDriver{podConfigStore: mustNewPodConfigStore(), netdb: newFakeInventoryDB()}
	podUID := types.UID("pod-a")
	cfg := testVfioDeviceConfig("vfio.petasus.io/vfio=uid_nic_dev")
	cfg.VfioPCIAddress = "0000:01:08.2"
	if err := np.podConfigStore.SetDeviceConfig(podUID, "pci-0000-01-08-2", cfg); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}
	result := resourceapi.DeviceRequestAllocationResult{Device: "pci-0000-01-08-2", Request: "nic"}
	handled, err := np.prepareCarryDevice(context.Background(), &resourceapi.ResourceClaim{}, podUID, result, &carryClaim{})
	if !handled || err == nil {
		t.Errorf("prepareCarryDevice() = %v, %v; want handled with an error", handled, err)
	}
	handled, _ = np.prepareCarryDevice(context.Background(), &resourceapi.ResourceClaim{}, "pod-b", result, &carryClaim{})
	if handled {
		t.Error("a device no pod prepared went to the vfio path")
	}
}

// A claim whose VF was reallocated before its unprepare leaves the VF to the
// new claim; the last owner restores it, and a failed restore fails only the
// first unprepare.
func TestUnprepareRestoresOnlyTheLastOwner(t *testing.T) {
	np := &NetworkDriver{
		podConfigStore: mustNewPodConfigStore(),
		carryState: carryState{
			vfioConfigurer:     vfio.NewConfigurer(nil, nil, ""),
			vfioCDI:            &cdi.Writer{DriverName: "vfio.petasus.io", SpecDir: t.TempDir()},
			vfioRestoreRetried: set.New[types.UID](),
		},
	}
	// An unknown mode makes Restore fail without touching the host.
	failing := &vfio.HostState{Mode: "unknown", VfPci: "0000:01:08.2"}
	for pod, claim := range map[types.UID]string{"old-pod": "old-nic", "new-pod": "new-nic"} {
		if err := np.podConfigStore.SetDeviceConfig(pod, "pci-0000-01-08-2", DeviceConfig{
			Claim:             types.NamespacedName{Namespace: "ns", Name: claim},
			CarryDeviceConfig: CarryDeviceConfig{VfioPCIAddress: "0000:01:08.2", VfioState: failing},
		}); err != nil {
			t.Fatal(err)
		}
	}
	unprepare := func(name string) error {
		return np.unprepareResourceClaim(context.Background(), kubeletplugin.NamespacedObject{
			NamespacedName: types.NamespacedName{Namespace: "ns", Name: name}, UID: types.UID(name + "-uid"),
		})
	}

	if err := unprepare("old-nic"); err != nil {
		t.Fatalf("unprepare of the claim the VF moved away from restored it: %v", err)
	}
	if err := unprepare("new-nic"); err == nil {
		t.Fatal("first unprepare with a failed restore succeeded, want an error so kubelet retries")
	}
	if _, ok := np.podConfigStore.GetDeviceConfig("new-pod", "pci-0000-01-08-2"); !ok {
		t.Fatal("a failed unprepare dropped the entry it has to retry")
	}
	if err := unprepare("new-nic"); err != nil {
		t.Fatalf("second unprepare = %v, want the claim let go", err)
	}
	if _, ok := np.podConfigStore.GetDeviceConfig("new-pod", "pci-0000-01-08-2"); ok {
		t.Fatal("the claim's entry is still stored after it was let go")
	}
}

// A claim whose checkpoint cannot be deleted fails its unprepare and keeps
// its entry: kubelet retries, and a restart never reloads an owner of the VF
// that kubelet considers gone.
func TestUnprepareFailsWhileTheCheckpointKeepsTheClaim(t *testing.T) {
	cp := &failingDeleteCheckpointer{Checkpointer: newTestBoltCheckpointer(t), err: fmt.Errorf("disk full")}
	store, err := newPodConfigStoreWithCheckpointer(cp)
	if err != nil {
		t.Fatal(err)
	}
	np := &NetworkDriver{
		podConfigStore: store,
		carryState: carryState{
			vfioConfigurer:     vfio.NewConfigurer(nil, nil, ""),
			vfioCDI:            &cdi.Writer{DriverName: "vfio.petasus.io", SpecDir: t.TempDir()},
			vfioRestoreRetried: set.New[types.UID](),
		},
	}
	if err := store.SetDeviceConfig("pod-a", "pci-0000-01-08-2", DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "vm-nic"},
		CarryDeviceConfig: CarryDeviceConfig{
			VfioPCIAddress: "0000:01:08.2",
			VfioState:      &vfio.HostState{Mode: vfio.ModePassthrough, VfPci: "0000:01:08.2"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	claim := kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vm-nic"},
		UID:            "claim-uid",
	}

	if err := np.unprepareResourceClaim(context.Background(), claim); err == nil {
		t.Fatal("unprepare succeeded although the checkpoint kept the claim")
	}
	if _, ok := store.GetDeviceConfig("pod-a", "pci-0000-01-08-2"); !ok {
		t.Fatal("the failed unprepare dropped the entry it has to retry")
	}
	cp.err = nil
	if err := np.unprepareResourceClaim(context.Background(), claim); err != nil {
		t.Fatalf("retried unprepare: %v", err)
	}
	if _, ok := store.GetDeviceConfig("pod-a", "pci-0000-01-08-2"); ok {
		t.Fatal("the claim's entry is still stored after the retry")
	}
}

// metadataPlugin prepares every claim with one device carrying metadata, as
// prepareVfioDevice does.
type metadataPlugin struct{}

func (metadataPlugin) PrepareResourceClaims(_ context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	pci := "0000:01:08.2"
	results := map[types.UID]kubeletplugin.PrepareResult{}
	for _, claim := range claims {
		results[claim.UID] = kubeletplugin.PrepareResult{Devices: []kubeletplugin.Device{{
			Requests:     []string{"nic"},
			PoolName:     "node1",
			DeviceName:   "pci-0000-01-08-2",
			CDIDeviceIDs: []string{"vfio.petasus.io/vfio=" + string(claim.UID) + "_nic_pci-0000-01-08-2"},
			Metadata: &kubeletplugin.DeviceMetadata{Attributes: map[string]resourceapi.DeviceAttribute{
				"resource.kubernetes.io/pciBusID": {StringValue: &pci},
			}},
		}}}
	}
	return results, nil
}

func (metadataPlugin) UnprepareResourceClaims(context.Context, []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	return nil, nil
}

func (metadataPlugin) HandleError(context.Context, error, string) {}

func (metadataPlugin) WatchHealthStatus(ctx context.Context, _ chan<- kubeletplugin.DeviceHealthReport) error {
	<-ctx.Done()
	return nil
}

// The helper only logs a failed metadata write; with this driver's options the
// claim fails prepare instead of staying prepared without the PCI address
// virt-launcher reads.
func TestPrepareFailsWhenTheDeviceMetadataIsNotWritten(t *testing.T) {
	for name, writeErr := range map[string]error{"written": nil, "full": syscall.ENOSPC} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			claim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vm-nic", UID: "claim-uid"},
				Status:     resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{}},
			}
			np := &NetworkDriver{driverName: "vfio.petasus.io", carryState: carryState{cdiDir: filepath.Join(dir, "cdi")}}
			opts := append(np.carryKubeletOptions(),
				kubeletplugin.DriverName(np.driverName),
				kubeletplugin.NodeName("node1"),
				kubeletplugin.KubeClient(fake.NewClientset(claim)),
				kubeletplugin.RegistrationService(false),
				kubeletplugin.PluginDataDirectoryPath(dir),
				kubeletplugin.MetadataFileOps(kubeletplugin.MetadataFileOperations{
					WriteFile: func(name string, data []byte, perm os.FileMode) error {
						if writeErr != nil {
							return writeErr
						}
						return os.WriteFile(name, data, perm)
					},
				}),
			)
			helper, err := kubeletplugin.Start(t.Context(), metadataPlugin{}, opts...)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer helper.Stop()
			conn, err := grpc.NewClient("unix://"+filepath.Join(dir, "dra.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			resp, err := drapbv1.NewDRAPluginClient(conn).NodePrepareResources(t.Context(), &drapbv1.NodePrepareResourcesRequest{
				Claims: []*drapbv1.Claim{{Namespace: "ns", Name: "vm-nic", Uid: "claim-uid"}},
			})
			if err != nil {
				t.Fatalf("NodePrepareResources: %v", err)
			}
			got := resp.GetClaims()["claim-uid"]
			if writeErr != nil {
				if !strings.Contains(got.GetError(), "metadata was not written") {
					t.Fatalf("claim result %v, want a metadata error", got)
				}
				return
			}
			if got.GetError() != "" || len(got.GetDevices()) != 1 {
				t.Fatalf("claim result %v, want the device prepared", got)
			}
			if ids := got.GetDevices()[0].GetCdiDeviceIds(); !slices.Contains(ids, "vfio.petasus.io/metadata=claim-uid_nic") {
				t.Fatalf("CDI devices %v lack the metadata device", ids)
			}
		})
	}
}

// A kubelet on the v1beta1 API gets the same per-claim failure; a claim
// already failed, or prepared without devices, is left alone.
func TestRequireDeviceMetadataV1beta1(t *testing.T) {
	device := func(ids ...string) []*drapbv1beta1.Device {
		return []*drapbv1beta1.Device{{RequestNames: []string{"nic"}, CdiDeviceIds: ids}}
	}
	resp := &drapbv1beta1.NodePrepareResourcesResponse{Claims: map[string]*drapbv1beta1.NodePrepareResourceResponse{
		"missing": {Devices: device("vfio.petasus.io/vfio=missing_nic_dev")},
		"written": {Devices: device("vfio.petasus.io/vfio=written_nic_dev", "vfio.petasus.io/metadata=written_nic")},
		"failed":  {Error: "prepare failed"},
		"empty":   {},
	}}
	handler := func(context.Context, any) (any, error) { return resp, nil }
	if _, err := requireDeviceMetadata("vfio.petasus.io")(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatal(err)
	}
	for uid, want := range map[string]string{"missing": "metadata was not written", "written": "", "failed": "prepare failed", "empty": ""} {
		got := resp.Claims[uid].GetError()
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("claim %s: error %q, want %q", uid, got, want)
		}
	}
}
