package main

import (
	"log/slog"
	"testing"

	baremetal "github.com/scaleway/scaleway-sdk-go/api/baremetal/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// TestNewSCWReal_BuildsElasticMetalClient locks the wiring: a BARE_METAL config
// now constructs the real Elastic Metal client instead of the old loud rejection.
func TestNewSCWReal_BuildsElasticMetalClient(t *testing.T) {
	logger := slog.Default()
	cfg := scwRealConfig{
		Creds: scwCredentials{
			accessKey: "SCWABCDEFGHIJKLMNOPQ",
			secretKey: "11111111-1111-1111-1111-111111111111",
			projectID: "22222222-2222-2222-2222-222222222222",
			region:    "fr-par-1",
		},
		CommercialKind:    providerkit.CapacityBareMetal,
		Image:             "Ubuntu 22.04",
		Zone:              "fr-par-1",
		Vault:             newBootstrapVault([]byte("test-secret"), logger),
		BootstrapEndpoint: "https://scaleway-provider.example:9443",
	}
	client, err := newSCWReal(cfg, logger)
	if err != nil {
		t.Fatalf("newSCWReal(bare_metal): unexpected error: %v", err)
	}
	if _, ok := client.(*scwBaremetal); !ok {
		t.Fatalf("newSCWReal(bare_metal) = %T, want *scwBaremetal", client)
	}
}

// TestOfferCapacity_SumsHardware verifies allocatable is summed across an offer's
// multiple CPUs/DIMMs and counts GPUs (a baremetal server can carry several).
func TestOfferCapacity_SumsHardware(t *testing.T) {
	o := &baremetal.Offer{
		CPUs:     []*baremetal.CPU{{CoreCount: 8}, {CoreCount: 8}},
		Memories: []*baremetal.Memory{{Capacity: scw.Size(32 * 1024 * 1024 * 1024)}}, // 32 GiB
		Gpus:     []*baremetal.GPU{{Name: "L4"}},
	}
	got := offerCapacity(o)
	if got.VCPU != 16 {
		t.Errorf("VCPU = %d, want 16", got.VCPU)
	}
	if got.MemMiB != 32*1024 {
		t.Errorf("MemMiB = %d, want %d", got.MemMiB, 32*1024)
	}
	if got.GPUs != 1 {
		t.Errorf("GPUs = %d, want 1", got.GPUs)
	}
}

// TestIsUp maps baremetal power states to the reachable/Running flag.
func TestIsUp(t *testing.T) {
	up := []baremetal.ServerStatus{baremetal.ServerStatusReady, baremetal.ServerStatusStarting}
	down := []baremetal.ServerStatus{
		baremetal.ServerStatusStopped, baremetal.ServerStatusStopping,
		baremetal.ServerStatusDelivering, baremetal.ServerStatusError,
		baremetal.ServerStatusOutOfStock, baremetal.ServerStatusDeleting,
	}
	for _, s := range up {
		if !isUp(s) {
			t.Errorf("isUp(%q) = false, want true", s)
		}
	}
	for _, s := range down {
		if isUp(s) {
			t.Errorf("isUp(%q) = true, want false", s)
		}
	}
}
