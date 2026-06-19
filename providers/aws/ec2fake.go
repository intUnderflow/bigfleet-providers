package main

import (
	"context"
	"fmt"
	"sync"
)

// ec2Fake is an in-memory ec2Client. It is NOT a production artifact — it
// backs unit tests and credential-free conformance runs (`--ec2-backend=fake`,
// or `auto` with no `--region`). It models just enough EC2 behaviour for the
// lifecycle: launch returns a synthetic instance id, terminate removes it,
// describe lists the live ones, and bind/drain toggle the cluster tag.
type ec2Fake struct {
	mu        sync.Mutex
	seq       int
	instances map[string]*ec2Instance // keyed by instance id
	byToken   map[string]string       // ClientToken -> instance id (EC2 idempotency)
	// spotUSD is the deterministic spot price the simulator reports, so
	// conformance and tests are reproducible.
	spotUSD float64
}

func newEC2Fake() *ec2Fake {
	return &ec2Fake{
		instances: make(map[string]*ec2Instance),
		byToken:   make(map[string]string),
		spotUSD:   0.0345,
	}
}

func (f *ec2Fake) RunInstance(_ context.Context, spec runSpec) (ec2Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model EC2 ClientToken idempotency: a repeated token returns the existing
	// instance instead of launching a second one.
	if spec.IdempotencyToken != "" {
		if id, ok := f.byToken[spec.IdempotencyToken]; ok {
			if inst, ok := f.instances[id]; ok {
				return *inst, nil
			}
		}
	}
	f.seq++
	id := fmt.Sprintf("i-fake%08d", f.seq)
	inst := &ec2Instance{
		InstanceID:   id,
		MachineID:    spec.MachineID,
		InstanceType: spec.InstanceType,
		Zone:         spec.Zone,
		Spot:         spec.Spot,
		Capacity:     spec.Capacity,
		PrivateDNS:   fmt.Sprintf("ip-10-0-0-%d.ec2.internal", f.seq%250+1),
		Running:      true,
	}
	f.instances[id] = inst
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = id
	}
	return *inst, nil
}

func (f *ec2Fake) TerminateInstance(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.instances[instanceID]; !ok {
		return fmt.Errorf("ec2fake: terminate unknown instance %q", instanceID)
	}
	delete(f.instances, instanceID)
	return nil
}

func (f *ec2Fake) DescribeManaged(_ context.Context) ([]ec2Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ec2Instance, 0, len(f.instances))
	for _, inst := range f.instances {
		out = append(out, *inst)
	}
	return out, nil
}

func (f *ec2Fake) ApplyBootstrap(_ context.Context, instanceID, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[instanceID]
	if !ok {
		return fmt.Errorf("ec2fake: configure unknown instance %q", instanceID)
	}
	inst.ClusterID = clusterID
	return nil
}

func (f *ec2Fake) DrainNode(_ context.Context, instanceID string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[instanceID]
	if !ok {
		return fmt.Errorf("ec2fake: drain unknown instance %q", instanceID)
	}
	inst.ClusterID = ""
	return nil
}

func (f *ec2Fake) SpotPriceUSD(_ context.Context, _, _ string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spotUSD, nil
}

var _ ec2Client = (*ec2Fake)(nil)
