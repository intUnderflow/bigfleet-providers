package main

import (
	"context"
	"fmt"
	"sync"
)

// proxmoxFake is an in-memory proxmoxClient. It is NOT a production artifact — it
// backs unit tests and the credential-free conformance / certification run
// (`--proxmox-backend=fake`, or `auto` with no --proxmox-api-url). It models just
// enough Proxmox behaviour for the lifecycle: clone allocates+starts a synthetic
// VM, delete removes it, describe lists the live ones, and bind/drain toggle the
// cluster binding.
//
// It is the default main.go backend when no Proxmox API URL is supplied, so the
// certification harness (which boots the binary with no credential flag) gets a
// working endpoint with zero hypervisor.
type proxmoxFake struct {
	mu        sync.Mutex
	nextVMID  int
	vms       map[string]*vmInstance // keyed by "<node>/<vmid>"
	byMachine map[string]string      // machine id -> "<node>/<vmid>" key
}

func newProxmoxFake() *proxmoxFake {
	return &proxmoxFake{
		nextVMID:  100,
		vms:       make(map[string]*vmInstance),
		byMachine: make(map[string]string),
	}
}

func fakeKey(node string, vmid int) string { return fmt.Sprintf("%s/%d", node, vmid) }

// CloneVM models a tag-keyed idempotent clone: a repeated clone for the same
// machine id adopts the existing VM (re-powering it if it shut off out of band)
// instead of cloning a second one — the no-double-clone invariant.
func (f *proxmoxFake) CloneVM(_ context.Context, spec vmSpec) (vmInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec.MachineID != "" {
		if key, ok := f.byMachine[spec.MachineID]; ok {
			if vm, ok := f.vms[key]; ok {
				// Adopt-on-retry: a VM that shut off out of band is powered back
				// on, modelling the real client's EnsureRunning adopt branch.
				vm.Running = true
				return *vm, nil
			}
		}
	}
	f.nextVMID++
	vmid := f.nextVMID
	vm := &vmInstance{
		Node:      spec.Zone,
		VMID:      vmid,
		Name:      fmt.Sprintf("bigfleet-%d", vmid),
		MachineID: spec.MachineID,
		Running:   true,
	}
	key := fakeKey(spec.Zone, vmid)
	f.vms[key] = vm
	if spec.MachineID != "" {
		f.byMachine[spec.MachineID] = key
	}
	return *vm, nil
}

// DeleteVM is idempotent, matching the real client: destroying an already-gone
// VM succeeds, so a Delete after an out-of-band teardown never spuriously fails
// the machine.
func (f *proxmoxFake) DeleteVM(_ context.Context, node string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeKey(node, vmid)
	if vm, ok := f.vms[key]; ok {
		delete(f.byMachine, vm.MachineID)
	}
	delete(f.vms, key)
	return nil
}

func (f *proxmoxFake) DescribeManaged(_ context.Context) ([]vmInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vmInstance, 0, len(f.vms))
	for _, vm := range f.vms {
		out = append(out, *vm)
	}
	return out, nil
}

// EnsureRunning powers a stopped fake VM back on, modelling the real client's
// heal of an out-of-band stop before Configure/Drain.
func (f *proxmoxFake) EnsureRunning(_ context.Context, vm vmInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[fakeKey(vm.Node, vm.VMID)]
	if !ok {
		return fmt.Errorf("proxmoxfake: ensure-running unknown VM %q", vm.hostRef())
	}
	v.Running = true
	return nil
}

func (f *proxmoxFake) ApplyBootstrap(_ context.Context, vm vmInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[fakeKey(vm.Node, vm.VMID)]
	if !ok {
		return fmt.Errorf("proxmoxfake: configure unknown VM %q", vm.hostRef())
	}
	// The real guest-agent bootstrap can't run against a stopped VM, so reject it
	// here too — a Configure that reaches this on a stopped VM means the
	// EnsureRunning heal was skipped.
	if !v.Running {
		return fmt.Errorf("proxmoxfake: configure stopped VM %q (EnsureRunning not called first)", vm.hostRef())
	}
	v.ClusterID = clusterID
	return nil
}

func (f *proxmoxFake) DrainNode(_ context.Context, vm vmInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[fakeKey(vm.Node, vm.VMID)]
	if !ok {
		return fmt.Errorf("proxmoxfake: drain unknown VM %q", vm.hostRef())
	}
	if !v.Running {
		return fmt.Errorf("proxmoxfake: drain stopped VM %q (EnsureRunning not called first)", vm.hostRef())
	}
	v.ClusterID = ""
	return nil
}

// setRunning flips a VM's power state, modelling a tagged VM that has been
// stopped out of band (host power-cycle, an HA stop, an in-guest poweroff).
// Test-only: it lets a test drive Describe/Configure with a stopped managed VM
// and assert EnsureRunning healed it. Returns false if the VM is unknown.
func (f *proxmoxFake) setRunning(node string, vmid int, running bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[fakeKey(node, vmid)]
	if !ok {
		return false
	}
	v.Running = running
	return true
}

func (f *proxmoxFake) Close() error { return nil }

var _ proxmoxClient = (*proxmoxFake)(nil)
