package main

import (
	"context"
	"fmt"
	"sync"
)

// libvirtFake is an in-memory libvirtClient. It is NOT a production artifact — it
// backs unit tests and credential-free conformance / certification runs
// (`--libvirt-backend=fake`, or `auto` with no --connect host). It models just
// enough libvirt behaviour for the lifecycle: create defines+starts a synthetic
// domain, delete removes it, describe lists the live ones, and bind/drain toggle
// the cluster binding.
//
// The default main.go backend when no libvirt host is supplied, so the
// certification harness (which boots the binary with no credential flag) gets a
// working endpoint with zero hypervisor.
type libvirtFake struct {
	mu      sync.Mutex
	seq     int
	domains map[string]*domainInstance // keyed by "<zone>/<domain>"
	byToken map[string]string          // idempotency token -> "<zone>/<domain>" key
}

func newLibvirtFake() *libvirtFake {
	return &libvirtFake{
		domains: make(map[string]*domainInstance),
		byToken: make(map[string]string),
	}
}

func fakeKey(zone, domain string) string { return zone + "/" + domain }

func (f *libvirtFake) CreateDomain(_ context.Context, spec domainSpec) (domainInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model define idempotency: a repeated token returns the existing domain
	// instead of defining a second one.
	if spec.IdempotencyToken != "" {
		if key, ok := f.byToken[spec.IdempotencyToken]; ok {
			if dom, ok := f.domains[key]; ok {
				// Recover-on-retry: a domain that shut off out of band is powered
				// back on, modelling the real client's recoverDomain branch.
				dom.Running = true
				return *dom, nil
			}
		}
	}
	f.seq++
	name := fmt.Sprintf("bigfleet-%06d", f.seq)
	dom := &domainInstance{
		Zone:       spec.Zone,
		DomainName: name,
		UUID:       fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq),
		MachineID:  spec.MachineID,
		Running:    true,
	}
	key := fakeKey(spec.Zone, name)
	f.domains[key] = dom
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = key
	}
	return *dom, nil
}

func (f *libvirtFake) DeleteDomain(_ context.Context, zone, domainName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client: destroying+undefining an already-gone
	// domain succeeds, so a Delete after an out-of-band teardown never spuriously
	// fails the machine.
	delete(f.domains, fakeKey(zone, domainName))
	return nil
}

func (f *libvirtFake) DescribeManaged(_ context.Context) ([]domainInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domainInstance, 0, len(f.domains))
	for _, dom := range f.domains {
		out = append(out, *dom)
	}
	return out, nil
}

// EnsureRunning powers a stopped fake domain back on, modelling the real
// client's heal of an out-of-band poweroff before Configure/Drain.
func (f *libvirtFake) EnsureRunning(_ context.Context, dom domainInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[fakeKey(dom.Zone, dom.DomainName)]
	if !ok {
		return fmt.Errorf("libvirtfake: ensure-running unknown domain %q", dom.hostRef())
	}
	d.Running = true
	return nil
}

func (f *libvirtFake) ApplyBootstrap(_ context.Context, dom domainInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[fakeKey(dom.Zone, dom.DomainName)]
	if !ok {
		return fmt.Errorf("libvirtfake: configure unknown domain %q", dom.hostRef())
	}
	// The real guest-agent bootstrap can't run against a stopped domain, so reject
	// it here too — a Configure that reaches this on a shut-off domain means the
	// EnsureRunning heal was skipped.
	if !d.Running {
		return fmt.Errorf("libvirtfake: configure shut-off domain %q (EnsureRunning not called first)", dom.hostRef())
	}
	d.ClusterID = clusterID
	return nil
}

func (f *libvirtFake) DrainNode(_ context.Context, dom domainInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[fakeKey(dom.Zone, dom.DomainName)]
	if !ok {
		return fmt.Errorf("libvirtfake: drain unknown domain %q", dom.hostRef())
	}
	if !d.Running {
		return fmt.Errorf("libvirtfake: drain shut-off domain %q (EnsureRunning not called first)", dom.hostRef())
	}
	d.ClusterID = ""
	return nil
}

// setRunning flips a domain's power state, modelling a tagged domain that has
// been shut off out of band (host reboot without autostart, in-guest poweroff).
// Test-only: it lets a test drive Describe with a shut-off managed domain and
// assert the slot is not advertised Idle. Returns false if the domain is unknown.
func (f *libvirtFake) setRunning(zone, domainName string, running bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[fakeKey(zone, domainName)]
	if !ok {
		return false
	}
	d.Running = running
	return true
}

func (f *libvirtFake) Close() error { return nil }

var _ libvirtClient = (*libvirtFake)(nil)
