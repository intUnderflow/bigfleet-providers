package main

import (
	"context"
	"fmt"
	"sync"
)

// ociFake is an in-memory ociClient. It is NOT a production artifact — it backs
// unit tests and credential-free conformance/certification runs
// (--oci-backend=fake, or `auto` with no region/compartment). It models just
// enough OCI Compute behaviour for the lifecycle: launch returns a synthetic
// OCID, terminate removes it, describe lists the live ones, and bind/drain toggle
// the cluster tag.
type ociFake struct {
	mu        sync.Mutex
	seq       int
	instances map[string]*ociInstance // keyed by instance OCID
	byToken   map[string]string       // idempotency token -> OCID
}

func newOCIFake() *ociFake {
	return &ociFake{
		instances: make(map[string]*ociInstance),
		byToken:   make(map[string]string),
	}
}

func (f *ociFake) LaunchInstance(_ context.Context, spec launchSpec) (ociInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model launch idempotency: a repeated token returns the existing instance
	// instead of launching a second one.
	if spec.IdempotencyToken != "" {
		if id, ok := f.byToken[spec.IdempotencyToken]; ok {
			if inst, ok := f.instances[id]; ok {
				return *inst, nil
			}
		}
	}
	f.seq++
	ocid := fmt.Sprintf("ocid1.instance.oc1..fake%08d", f.seq)
	inst := &ociInstance{
		InstanceID:         ocid,
		MachineID:          spec.MachineID,
		Shape:              spec.Shape,
		AvailabilityDomain: spec.AvailabilityDomain,
		OCPUs:              spec.OCPUs,
		MemoryGB:           spec.MemoryGB,
		Preemptible:        spec.Preemptible,
		Capacity:           spec.Capacity,
		PrivateIP:          fmt.Sprintf("10.0.%d.%d", f.seq/250%250, f.seq%250+1),
		Running:            true,
	}
	f.instances[ocid] = inst
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = ocid
	}
	return *inst, nil
}

func (f *ociFake) EnsureRunning(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst, ok := f.instances[instanceID]; ok {
		inst.Running = true
	}
	return nil
}

func (f *ociFake) TerminateInstance(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client (terminating an already-gone instance
	// is a no-op): a Delete after an out-of-band terminate never spuriously fails
	// the machine.
	delete(f.instances, instanceID)
	return nil
}

func (f *ociFake) DescribeManaged(_ context.Context) ([]ociInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ociInstance, 0, len(f.instances))
	for _, inst := range f.instances {
		out = append(out, *inst)
	}
	return out, nil
}

func (f *ociFake) ApplyBootstrap(_ context.Context, inst ociInstance, clusterID string, _ []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.instances[inst.InstanceID]
	if !ok {
		return fmt.Errorf("ocifake: configure unknown instance %q", inst.InstanceID)
	}
	s.ClusterID = clusterID
	return nil
}

func (f *ociFake) DrainNode(_ context.Context, inst ociInstance, _ int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.instances[inst.InstanceID]
	if !ok {
		return fmt.Errorf("ocifake: drain unknown instance %q", inst.InstanceID)
	}
	s.ClusterID = ""
	return nil
}

var _ ociClient = (*ociFake)(nil)
