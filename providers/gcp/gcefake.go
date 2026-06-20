package main

import (
	"context"
	"fmt"
	"sync"
)

// gceFake is an in-memory gceClient. It is NOT a production artifact — it backs
// unit tests and credential-free certification runs (`--gcp-backend=fake`, or
// `auto` with no `--region`). It models just enough GCE behaviour for the
// lifecycle: insert returns a synthetic instance, delete removes it, describe
// lists the live ones, and bind/drain toggle the cluster label.
type gceFake struct {
	mu        sync.Mutex
	seq       int
	instances map[string]*gceInstance // keyed by "zone/name"
	byToken   map[string]string       // idempotency token -> "zone/name"
}

func newGCEFake() *gceFake {
	return &gceFake{
		instances: make(map[string]*gceInstance),
		byToken:   make(map[string]string),
	}
}

func fakeKey(zone, name string) string { return zone + "/" + name }

func (f *gceFake) Insert(_ context.Context, spec instanceSpec) (gceInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model GCE create idempotency: a repeated token returns the existing
	// instance instead of launching a second one (the real client achieves the
	// same via a stable, token-derived instance name).
	if spec.IdempotencyToken != "" {
		if key, ok := f.byToken[spec.IdempotencyToken]; ok {
			if inst, ok := f.instances[key]; ok {
				return *inst, nil
			}
		}
	}
	f.seq++
	name := instanceName(spec)
	if name == "" {
		name = fmt.Sprintf("bigfleet-fake-%08d", f.seq)
	}
	inst := &gceInstance{
		Name:        name,
		Zone:        spec.Zone,
		MachineID:   spec.MachineID,
		MachineType: spec.MachineType,
		Spot:        spec.Spot,
		Capacity:    spec.Capacity,
		SelfLink:    fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/fake/zones/%s/instances/%s", spec.Zone, name),
		Running:     true,
	}
	key := fakeKey(spec.Zone, name)
	f.instances[key] = inst
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = key
	}
	return *inst, nil
}

func (f *gceFake) DeleteInstance(_ context.Context, zone, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client: deleting an unknown / already-gone
	// instance succeeds, so a Delete after an out-of-band deletion never
	// spuriously fails the machine.
	delete(f.instances, fakeKey(zone, name))
	return nil
}

func (f *gceFake) DescribeManaged(_ context.Context) ([]gceInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gceInstance, 0, len(f.instances))
	for _, inst := range f.instances {
		out = append(out, *inst)
	}
	return out, nil
}

func (f *gceFake) ApplyBootstrap(_ context.Context, inst gceInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.instances[fakeKey(inst.Zone, inst.Name)]
	if !ok {
		return fmt.Errorf("gcefake: configure unknown instance %q", fakeKey(inst.Zone, inst.Name))
	}
	s.ClusterID = clusterID
	return nil
}

func (f *gceFake) DrainNode(_ context.Context, inst gceInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.instances[fakeKey(inst.Zone, inst.Name)]
	if !ok {
		return fmt.Errorf("gcefake: drain unknown instance %q", fakeKey(inst.Zone, inst.Name))
	}
	s.ClusterID = ""
	return nil
}

// DescribeMachineTypeCapacities resolves capacities from the pinned table, so
// the simulator (and credential-free certification) exercises the resolve path
// deterministically. Types absent from the table are omitted, exactly as a real
// MachineTypes.Get omits a type unavailable in the zone.
func (f *gceFake) DescribeMachineTypeCapacities(_ context.Context, refs []machineTypeRef) (map[string]machineCapacity, error) {
	out := make(map[string]machineCapacity, len(refs))
	for _, ref := range refs {
		if c, ok := machineTypeTable[ref.MachineType]; ok {
			out[ref.MachineType] = c
		}
	}
	return out, nil
}

var _ gceClient = (*gceFake)(nil)
