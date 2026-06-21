package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// proxmoxClient is the entire Proxmox VE substrate surface the [proxmoxBackend]
// drives. It is deliberately small and substrate-shaped (not BigFleet-shaped) —
// providerkit owns every cross-cutting concern (fencing, idempotency, async
// dispatch, transition timeouts, shard_metadata, field-shape), so this is the
// only place Proxmox appears.
//
// Two implementations ship:
//   - proxmoxReal (proxmoxreal.go) wraps the pure-Go go-proxmox client against
//     the cluster's /api2/json REST API — the production client.
//   - proxmoxFake (proxmoxfake.go) is an in-memory simulator that backs unit
//     tests and the credential-free conformance / certification run.
//
// A "machine" is a Proxmox qemu VM (one VMID on one cluster node). Every method
// is scoped to the VMs this provider process manages (those tagged for it).
type proxmoxClient interface {
	// CloneVM clones the offering's template into a fresh VM on the target node
	// (spec.Zone), tags it for the BigFleet machine id, starts it, and waits
	// until it is running and the qemu guest agent is reachable. It returns the
	// VM's substrate identity.
	//
	// It MUST be idempotent on the machine id: a retried clone (after a
	// transport failure mid-clone) finds the VM already tagged for this machine
	// id and adopts it — re-powering it if it shut off out of band — rather than
	// cloning a second VM. This is what keeps a retried Create converging on ONE
	// VM (the no-double-clone invariant).
	CloneVM(ctx context.Context, spec vmSpec) (vmInstance, error)

	// DeleteVM stops the VM and destroys it together with its disks
	// (purge=1, destroy-unreferenced-disks=1), detaching it from any
	// HA/replication/backup references. The slot returns to Speculative.
	// Idempotent: deleting an already-gone VM (404 / not found) succeeds.
	DeleteVM(ctx context.Context, node string, vmid int) error

	// DescribeManaged returns every BigFleet-managed VM across the cluster (VMs
	// carrying this provider's marker tag), so a provider with no persisted
	// store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]vmInstance, error)

	// EnsureRunning powers a VM on if it is not already running and returns once
	// it is running AND the qemu guest agent is reachable, so Configure/Drain
	// never drive the guest agent against a stopped host. A VM the kit holds
	// Idle may have been stopped out of band (operator power-cycle, an HA event,
	// a maintenance reboot); the agent-delivered bootstrap/drain would otherwise
	// loop until the transition times out and FAILs. Idempotent: a VM that is
	// already running with a reachable agent is a no-op.
	EnsureRunning(ctx context.Context, vm vmInstance) error

	// ApplyBootstrap binds a running VM to a cluster and delivers the opaque
	// bootstrap blob over the qemu guest agent: it writes the blob into the
	// guest (agent/file-write) and runs the in-image bootstrap hook
	// (agent/exec), waiting for the hook to exit. A non-zero exit returns an
	// error (the kit drives the machine to FAILED). The blob is the kubelet
	// join data — never parse it.
	ApplyBootstrap(ctx context.Context, vm vmInstance, clusterID string, bootstrap []byte) error

	// DrainNode gracefully drains the kubelet off a running VM over the guest
	// agent, honouring the grace period, and clears its cluster binding —
	// leaving the VM running but unbound (Idle).
	DrainNode(ctx context.Context, vm vmInstance, gracePeriodSeconds int64) error

	// Close releases any client resources. Called once at shutdown.
	Close() error
}

// vmSpec is the clone request handed to CloneVM.
type vmSpec struct {
	MachineID    string
	InstanceType string // catalog name, e.g. pve.small
	Zone         string // the Proxmox node to place the clone on (target=)
	TemplateVMID int    // the source template VMID to clone
	Cores        int    // resolved from the instance-type catalog
	MemoryMiB    int64  // resolved from the instance-type catalog
	// IdempotencyToken is the kit's per-operation id. It is written into the
	// clone's Description alongside the machine-id tag so a retry maps to the
	// same VM; the fake uses it to model idempotent create.
	IdempotencyToken string
}

// vmInstance is the substrate view of one Proxmox VM, free of any go-proxmox
// types so the backend never sees the SDK.
type vmInstance struct {
	Node      string // the cluster node the VM lives on (the BigFleet zone)
	VMID      int    // the Proxmox VMID
	Name      string // the VM name
	MachineID string // bigfleet machine id, from the VM tag/description
	ClusterID string // bigfleet cluster binding, empty when unbound
	// Running reports whether the VM is in a live (running) state, as opposed to
	// stopped.
	Running bool
}

// hostRef is the substrate host id stamped on Machine.host.Ref: "<node>/<vmid>".
func (v vmInstance) hostRef() string { return v.Node + "/" + strconv.Itoa(v.VMID) }

// splitHostRef splits a "<node>/<vmid>" host ref into its parts. It splits on
// the LAST '/': a VMID is numeric and a node name never contains '/', so this
// round-trips correctly even if a node label itself ever contained '/'.
func splitHostRef(ref string) (node string, vmid int, ok bool) {
	i := strings.LastIndexByte(ref, '/')
	if i < 0 {
		return "", 0, false
	}
	node = ref[:i]
	id, err := strconv.Atoi(ref[i+1:])
	if err != nil || node == "" {
		return "", 0, false
	}
	return node, id, true
}

// machineIDTag is the per-machine marker tag written on a cloned VM:
// "bigfleet-<sanitized-machine-id>". Proxmox tags are restricted to
// [a-z0-9_-] (lowercased), so the raw machine id (which carries '/', '.', and
// uppercase) is sanitized for the tag; the unsanitized id is preserved verbatim
// in the VM Description for recovery. The tag is only used for cheap discovery
// and ownership checks, never to reconstruct the id.
func machineIDTag(machineID string) string {
	return tagPrefix + "-" + sanitizeTag(machineID)
}

const (
	// tagPrefix marks every VM this provider manages. It is a human-facing,
	// cheap-to-filter signal; CloneVM also stamps a per-machine machineIDTag.
	// NOTE: the pinned go-proxmox clone options cannot set tags atomically (tags
	// are applied AFTER the clone task completes), so discovery must NOT depend on
	// the tag alone — it also keys off vmNamePrefix + the Description, both of
	// which ARE set atomically at clone time. See DescribeManaged.
	tagPrefix = "bigfleet"
	// vmNamePrefix prefixes every cloned VM's name. Unlike the tag, the name is
	// part of the clone request, so it is present the instant the VM exists —
	// closing the window where a cloned-but-not-yet-tagged VM would be invisible
	// to retry discovery and get cloned a second time.
	vmNamePrefix = "bigfleet-"
)

// sanitizeTag maps an arbitrary string to the Proxmox tag charset ([a-z0-9_-]),
// lowercasing and replacing every other rune with '-'. It is deterministic so a
// given machine id always yields the same tag.
func sanitizeTag(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// machineIDDescription is the VM Description marker carrying the verbatim
// BigFleet machine id (and operation id), so DescribeManaged can recover the
// exact id even though the tag is sanitized.
func machineIDDescription(machineID, operationID string) string {
	return fmt.Sprintf("%s machine_id=%s operation_id=%s", descMarker, machineID, operationID)
}

const descMarker = "BigFleet managed VM."

// parseMachineIDDescription extracts the verbatim machine id from a VM
// Description written by machineIDDescription. Returns "" when the description
// does not carry the marker.
func parseMachineIDDescription(desc string) string {
	const key = "machine_id="
	i := strings.Index(desc, key)
	if i < 0 {
		return ""
	}
	rest := desc[i+len(key):]
	if j := strings.IndexAny(rest, " \t\r\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
