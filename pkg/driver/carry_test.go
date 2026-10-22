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
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"sigs.k8s.io/dranet/pkg/vfio"
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
		MetadataMount: &BindMount{
			HostPath:      "/var/lib/kubelet/plugins/vfio.petasus.io/dra-device-metadata/default_claim-1/nic/metadata.json",
			ContainerPath: "/var/run/kubernetes.io/dra-device-attributes/resourceclaims/claim-1/nic/vfio.petasus.io-metadata.json",
		},
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
