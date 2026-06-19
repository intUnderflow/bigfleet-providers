package main

import (
	"context"
	"fmt"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// templateBackend is a copy-me [providerkit.Backend]. It is deliberately the
// thinnest possible implementation: an in-memory stand-in that lets the
// conformance suite walk machines through the whole lifecycle end-to-end and
// proves the kit + template are contract-correct. To write a real provider,
// copy this directory and replace each TODO-marked method body with calls to
// your substrate's API.
//
// What you do NOT touch: fencing, idempotency, async dispatch, transition
// timeouts, the shard_metadata lifecycle, and Machine field-shape are all
// handled by providerkit.Server. Your Backend only ever speaks substrate.
//
// templateBackend also implements [providerkit.Deleter] (cloud-style: a host
// can be torn down and the slot returned to Speculative). A bare-metal
// free-pool provider should simply DELETE the DeleteInstance method below —
// the kit then answers Delete with codes.Unimplemented automatically.
type templateBackend struct {
	// providerName labels the HostRefs this provider hands out (shows up in
	// logs and in Machine.host.provider). Set it to something like
	// "example-eu-west-1".
	providerName string

	// seeds is the inventory Describe reports. The kit reads it once, on
	// first boot (empty store), to populate the authoritative inventory; on
	// restart the inventory is reloaded from the store instead. A real
	// provider's Describe would enumerate live substrate instances + quota.
	seeds []providerkit.Instance
}

// Describe reports the backend's current substrate inventory.
//
// TODO(provider-author): replace this with a real enumeration of your
// substrate — the quota/quota-slots you can actuate (returned as
// Speculative) plus any hosts that already exist (returned as Idle, e.g. a
// bare-metal free pool). Return substrate truth only; the kit owns lifecycle
// state, the cluster binding and shard_metadata.
func (b *templateBackend) Describe(_ context.Context) ([]providerkit.Instance, error) {
	return b.seeds, nil
}

// CreateInstance actuates a Speculative slot into a real, Idle host.
//
// TODO(provider-author): call your substrate's "launch instance" API here
// using the spec on req.Machine (InstanceType, Zone, CapacityType, …) and
// return the HostRef the new host is reachable at. Return an error to drive
// the machine to FAILED. Honour ctx — the kit cancels it when the Create
// transition timeout fires.
func (b *templateBackend) CreateInstance(_ context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	return providerkit.CreateInstanceResult{
		Host: providerkit.HostRef{Provider: b.providerName, Ref: "host-" + req.Machine.ID},
	}, nil
}

// ConfigureInstance injects the bootstrap blob and binds the host to a
// cluster.
//
// TODO(provider-author): apply req.BootstrapBlob to the host (cloud:
// user-data / ignition; bare-metal: PXE / iPXE) and join it to
// req.ClusterID. The blob is opaque — never parse it. Return an error to
// drive the machine to FAILED.
func (b *templateBackend) ConfigureInstance(_ context.Context, req providerkit.ConfigureInstanceRequest) error {
	if len(req.BootstrapBlob) == 0 {
		// The template does not require a blob; a real provider may. This is
		// just an example of substrate-side validation.
		return nil
	}
	return nil
}

// DrainInstance returns a Configured host to Idle, honouring the grace
// period.
//
// TODO(provider-author): cordon + drain the kubelet (honouring PDBs up to
// req.GracePeriodSeconds, then escalating) and detach the host from its
// cluster, leaving it running and unbound. Return an error to drive the
// machine to FAILED.
func (b *templateBackend) DrainInstance(_ context.Context, _ providerkit.DrainInstanceRequest) error {
	return nil
}

// DeleteInstance tears a host down and returns it to a Speculative slot.
//
// TODO(provider-author): call your substrate's "terminate instance" API. If
// your substrate has no meaningful teardown (bare-metal: the host returns to
// the free pool), DELETE this method entirely — the kit will then answer
// Delete with codes.Unimplemented, which is correct for fixed capacity.
func (b *templateBackend) DeleteInstance(_ context.Context, _ providerkit.DeleteInstanceRequest) error {
	return nil
}

// seedInventory builds count Speculative quota slots for the kit to seed on
// first boot, so a conformance run has machines to walk. It alternates
// on-demand and spot capacity to exercise the SPOT interruption-probability
// rule. A real provider does not need this — its Describe enumerates the
// substrate directly.
func seedInventory(count int, providerName string) []providerkit.Instance {
	out := make([]providerkit.Instance, 0, count)
	for i := 0; i < count; i++ {
		spot := i%3 == 0
		capType := providerkit.CapacityOnDemand
		prob := 0.0
		price := 0.40
		if spot {
			capType = providerkit.CapacitySpot
			prob = 0.05 // SPOT MUST declare a real interruption probability
			price = 0.12
		}
		out = append(out, providerkit.Instance{
			ID:                      fmt.Sprintf("%s-spec-%03d", providerName, i),
			State:                   providerkit.StateSpeculative,
			InstanceType:            "example-standard-4",
			Zone:                    "example-zone-a",
			CapacityType:            capType,
			PricePerHour:            price,
			InterruptionProbability: prob,
			Resources:               map[string]string{"cpu": "4", "memory": "16Gi"},
			Allocatable:             map[string]string{"cpu": "4", "memory": "16Gi"},
			Labels:                  map[string]string{"example.com/pool": "default"},
		})
	}
	return out
}

// Compile-time checks: the template is a full Backend and an optional
// Deleter. Remove the Deleter assertion (and DeleteInstance above) for a
// bare-metal provider.
var (
	_ providerkit.Backend = (*templateBackend)(nil)
	_ providerkit.Deleter = (*templateBackend)(nil)
)
