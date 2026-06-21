package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// proxmoxReal is the production proxmoxClient: a thin adapter over the pure-Go
// go-proxmox client against the cluster's /api2/json REST API. It owns the
// substrate truth (clone / start / stop / destroy + guest-agent file-write/exec)
// and translates it into the small vmInstance/vmSpec shapes the backend uses.
//
// One client against the cluster API endpoint is enough: the cluster API can
// reach every node, clones are placed with target=<node>, and node placement is
// read from Cluster.Resources. TLS is verified against the operator-supplied CA
// / pinned fingerprint by the *http.Client this is constructed with (see
// config.httpClient) — there is no skip-verify path.
type proxmoxReal struct {
	client *proxmox.Client
	pool   string // optional resource pool clones are placed in
	// bootstrapPath is the in-guest path the bootstrap blob is written to before
	// it is executed; bootstrapExec is the argv that runs it (the path is
	// appended as the final arg).
	bootstrapPath string
	bootstrapExec []string
	// agentTimeout bounds the wait for the guest agent to come up after a
	// start/clone; execTimeout bounds a single guest-agent exec; taskTimeout is a
	// generous backstop ceiling for a Proxmox task wait (clone/start/stop/delete)
	// — the real per-operation bound is the kit's transition ctx, which Wait
	// honours via its per-tick ping, so this only guards against a task that never
	// terminates and must exceed the longest transition budget (Drain, 15m).
	agentTimeout time.Duration
	execTimeout  time.Duration
	taskTimeout  time.Duration
	logger       *slog.Logger
}

type proxmoxRealConfig struct {
	Client        *proxmox.Client
	Pool          string
	BootstrapPath string
	BootstrapExec []string
	AgentTimeout  time.Duration
	ExecTimeout   time.Duration
	TaskTimeout   time.Duration
}

func newProxmoxReal(cfg proxmoxRealConfig, logger *slog.Logger) (*proxmoxReal, error) {
	if cfg.Client == nil {
		return nil, errors.New("proxmox real client: nil go-proxmox client")
	}
	path := cfg.BootstrapPath
	if path == "" {
		path = "/run/bigfleet-bootstrap"
	}
	exec := cfg.BootstrapExec
	if len(exec) == 0 {
		exec = []string{"/bin/sh"}
	}
	at := cfg.AgentTimeout
	if at <= 0 {
		at = 4 * time.Minute
	}
	et := cfg.ExecTimeout
	if et <= 0 {
		et = 5 * time.Minute
	}
	tt := cfg.TaskTimeout
	if tt <= 0 {
		tt = 30 * time.Minute
	}
	return &proxmoxReal{
		client:        cfg.Client,
		pool:          cfg.Pool,
		bootstrapPath: path,
		bootstrapExec: exec,
		agentTimeout:  at,
		execTimeout:   et,
		taskTimeout:   tt,
		logger:        logger,
	}, nil
}

// CloneVM clones the offering's template into a fresh VM on the target node and
// brings it up, idempotently. It first searches for a VM already tagged for this
// machine id and adopts it (re-powering it if stopped, waiting on an in-flight
// clone task implicitly via EnsureRunning) so a retried Create converges on ONE
// VM, never two.
func (r *proxmoxReal) CloneVM(ctx context.Context, spec vmSpec) (vmInstance, error) {
	// 1) Adopt an existing VM tagged for this machine id (the retry / recovery
	// path) rather than cloning a duplicate.
	if existing, ok, err := r.findByMachineID(ctx, spec.MachineID); err != nil {
		return vmInstance{}, err
	} else if ok {
		if err := r.EnsureRunning(ctx, existing); err != nil {
			return vmInstance{}, fmt.Errorf("adopt existing VM %s: %w", existing.hostRef(), err)
		}
		return r.describeVM(ctx, existing.Node, existing.VMID)
	}

	// 2) Locate the source template and clone it onto the target node.
	tmpl, err := r.findVM(ctx, spec.TemplateVMID)
	if err != nil {
		return vmInstance{}, fmt.Errorf("locate template VMID %d: %w", spec.TemplateVMID, err)
	}
	tmplVM, err := r.nodeVM(ctx, tmpl.Node, tmpl.VMID)
	if err != nil {
		return vmInstance{}, fmt.Errorf("open template VMID %d: %w", spec.TemplateVMID, err)
	}
	name := cloneName(spec.MachineID, spec.InstanceType)
	newid, task, err := tmplVM.Clone(ctx, &proxmox.VirtualMachineCloneOptions{
		Full:        true,
		Target:      spec.Zone,
		Name:        name,
		Pool:        r.pool,
		Description: machineIDDescription(spec.MachineID, spec.IdempotencyToken),
	})
	if err != nil {
		return vmInstance{}, fmt.Errorf("clone template %d -> node %s: %w", spec.TemplateVMID, spec.Zone, err)
	}
	if err := r.waitTask(ctx, task); err != nil {
		return vmInstance{}, fmt.Errorf("clone task: %w", err)
	}

	// 3) Tag + size the clone, then start it and wait for the guest agent.
	vm, err := r.nodeVM(ctx, spec.Zone, newid)
	if err != nil {
		return vmInstance{}, fmt.Errorf("open clone %s/%d: %w", spec.Zone, newid, err)
	}
	if err := r.configure(ctx, vm, spec); err != nil {
		return vmInstance{}, fmt.Errorf("configure clone %s/%d: %w", spec.Zone, newid, err)
	}
	inst := vmInstance{Node: spec.Zone, VMID: newid, Name: name, MachineID: spec.MachineID}
	if err := r.EnsureRunning(ctx, inst); err != nil {
		return vmInstance{}, fmt.Errorf("start clone %s/%d: %w", spec.Zone, newid, err)
	}
	return r.describeVM(ctx, spec.Zone, newid)
}

// configure applies the clone's tags, description, and sizing (cores/memory).
func (r *proxmoxReal) configure(ctx context.Context, vm *proxmox.VirtualMachine, spec vmSpec) error {
	opts := []proxmox.VirtualMachineOption{
		{Name: "cores", Value: spec.Cores},
		{Name: "memory", Value: spec.MemoryMiB},
		{Name: "tags", Value: strings.Join([]string{tagPrefix, machineIDTag(spec.MachineID)}, ";")},
		{Name: "description", Value: machineIDDescription(spec.MachineID, spec.IdempotencyToken)},
	}
	return vm.ConfigSync(ctx, opts...)
}

// DeleteVM stops then destroys the VM together with its disks (purge=1,
// destroy-unreferenced-disks=1). Idempotent: an already-gone VMID succeeds.
func (r *proxmoxReal) DeleteVM(ctx context.Context, node string, vmid int) error {
	vm, err := r.nodeVM(ctx, node, vmid)
	if err != nil {
		if isNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("open VM %s/%d: %w", node, vmid, err)
	}
	if !vm.IsStopped() {
		task, err := vm.Stop(ctx)
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("stop VM %s/%d: %w", node, vmid, err)
		}
		if err == nil {
			if werr := r.waitTask(ctx, task); werr != nil {
				return fmt.Errorf("stop task: %w", werr)
			}
		}
	}
	task, err := vm.Delete(ctx, &proxmox.VirtualMachineDeleteOptions{
		Purge:                    true,
		DestroyUnreferencedDisks: true,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("destroy VM %s/%d: %w", node, vmid, err)
	}
	if err := r.waitTask(ctx, task); err != nil {
		return fmt.Errorf("destroy task: %w", err)
	}
	return nil
}

// DescribeManaged returns every VM across the cluster carrying this provider's
// marker tag, recovering each one's verbatim machine id from its Description.
func (r *proxmoxReal) DescribeManaged(ctx context.Context) ([]vmInstance, error) {
	cluster, err := r.client.Cluster(ctx)
	if err != nil {
		return nil, fmt.Errorf("get cluster: %w", err)
	}
	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return nil, fmt.Errorf("list cluster VM resources: %w", err)
	}
	var out []vmInstance
	for _, res := range resources {
		if res.Type != "qemu" || res.Template != 0 {
			continue
		}
		// A VM is ours if it carries the marker tag OR its name has our prefix.
		// The name matters because the pinned SDK applies the tag only AFTER the
		// clone task completes, while the name is set in the clone request itself:
		// keying off the name closes the window where a cloned-but-not-yet-tagged
		// VM is invisible to retry discovery (which would clone a second VM and
		// leak the first one's disks). The Description (also atomic at clone)
		// carries the verbatim machine id.
		if !isManagedVM(res.Tags, res.Name) {
			continue
		}
		inst := vmInstance{
			Node:    res.Node,
			VMID:    int(res.VMID),
			Name:    res.Name,
			Running: res.Status == proxmox.StatusVirtualMachineRunning,
		}
		// Recover the verbatim machine id (and cluster binding) from the VM config
		// Description, which Cluster.Resources does not return. A name-prefixed VM
		// with no machine-id Description is surfaced as an untagged orphan
		// (MachineID == "") so it is never dropped.
		if full, err := r.describeVM(ctx, res.Node, int(res.VMID)); err == nil {
			inst.MachineID = full.MachineID
			inst.ClusterID = full.ClusterID
		} else if !isNotFound(err) {
			r.logger.Warn("describe managed VM: read config failed", "node", res.Node, "vmid", res.VMID, "err", err)
		}
		out = append(out, inst)
	}
	return out, nil
}

// EnsureRunning starts a stopped VM and waits until it is running and the guest
// agent is reachable. Idempotent: a running + agent-reachable VM is a no-op.
func (r *proxmoxReal) EnsureRunning(ctx context.Context, vmi vmInstance) error {
	vm, err := r.nodeVM(ctx, vmi.Node, vmi.VMID)
	if err != nil {
		return fmt.Errorf("open VM %s: %w", vmi.hostRef(), err)
	}
	if vm.IsStopped() {
		task, err := vm.Start(ctx)
		if err != nil {
			return fmt.Errorf("start VM %s: %w", vmi.hostRef(), err)
		}
		if err := r.waitTask(ctx, task); err != nil {
			return fmt.Errorf("start task for %s: %w", vmi.hostRef(), err)
		}
	}
	if err := vm.WaitForAgent(ctx, int(r.agentTimeout.Seconds())); err != nil {
		return fmt.Errorf("wait for guest agent on %s: %w", vmi.hostRef(), err)
	}
	return nil
}

// ApplyBootstrap writes the opaque bootstrap blob into the guest over the qemu
// guest agent (agent/file-write) and executes the in-image bootstrap hook
// (agent/exec), waiting for it to exit. A non-zero exit is an error. The blob is
// the kubelet join secret — it is never parsed, logged, or persisted.
func (r *proxmoxReal) ApplyBootstrap(ctx context.Context, vmi vmInstance, _ string, bootstrap []byte) error {
	vm, err := r.nodeVM(ctx, vmi.Node, vmi.VMID)
	if err != nil {
		return fmt.Errorf("open VM %s: %w", vmi.hostRef(), err)
	}
	if err := vm.AgentFileWrite(ctx, r.bootstrapPath, bootstrap); err != nil {
		return fmt.Errorf("write bootstrap to guest %s: %w", vmi.hostRef(), err)
	}
	argv := append(append([]string(nil), r.bootstrapExec...), r.bootstrapPath)
	if err := r.agentRun(ctx, vm, argv); err != nil {
		return fmt.Errorf("run bootstrap on guest %s: %w", vmi.hostRef(), err)
	}
	return nil
}

// DrainNode runs the in-image drain hook over the guest agent, bounded by the
// grace period, and clears the cluster binding (the kit clears cluster +
// shard_metadata on the Drain->Idle completion).
func (r *proxmoxReal) DrainNode(ctx context.Context, vmi vmInstance, gracePeriodSeconds int64) error {
	vm, err := r.nodeVM(ctx, vmi.Node, vmi.VMID)
	if err != nil {
		return fmt.Errorf("open VM %s: %w", vmi.hostRef(), err)
	}
	argv := []string{"/bin/sh", "-c", fmt.Sprintf("kubectl drain \"$(hostname)\" --ignore-daemonsets --delete-emptydir-data --grace-period=%d --timeout=%ds || true", gracePeriodSeconds, gracePeriodSeconds)}
	dctx := ctx
	if gracePeriodSeconds > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, time.Duration(gracePeriodSeconds)*time.Second+r.execTimeout)
		defer cancel()
	}
	return r.agentRun(dctx, vm, argv)
}

func (r *proxmoxReal) Close() error { return nil }

// --- helpers ---------------------------------------------------------------

// agentRun executes argv in the guest via the agent and waits for it to exit,
// returning an error on a non-zero exit code.
func (r *proxmoxReal) agentRun(ctx context.Context, vm *proxmox.VirtualMachine, argv []string) error {
	pid, err := vm.AgentExec(ctx, argv, "")
	if err != nil {
		return fmt.Errorf("agent exec: %w", err)
	}
	st, err := vm.WaitForAgentExecExit(ctx, pid, int(r.execTimeout.Seconds()))
	if err != nil {
		return fmt.Errorf("await agent exec: %w", err)
	}
	if st.ExitCode != 0 {
		return fmt.Errorf("guest command exited %d: %s", st.ExitCode, strings.TrimSpace(st.ErrData))
	}
	return nil
}

// describeVM reads one VM's full config and returns its substrate view including
// the verbatim machine id + cluster binding parsed from the Description.
func (r *proxmoxReal) describeVM(ctx context.Context, node string, vmid int) (vmInstance, error) {
	vm, err := r.nodeVM(ctx, node, vmid)
	if err != nil {
		return vmInstance{}, err
	}
	inst := vmInstance{
		Node:    node,
		VMID:    vmid,
		Name:    vm.Name,
		Running: vm.Status == proxmox.StatusVirtualMachineRunning,
	}
	if vm.VirtualMachineConfig != nil {
		inst.MachineID = parseMachineIDDescription(vm.VirtualMachineConfig.Description)
	}
	return inst, nil
}

// findByMachineID returns the managed VM tagged for the given machine id, if one
// exists.
func (r *proxmoxReal) findByMachineID(ctx context.Context, machineID string) (vmInstance, bool, error) {
	if machineID == "" {
		return vmInstance{}, false, nil
	}
	managed, err := r.DescribeManaged(ctx)
	if err != nil {
		return vmInstance{}, false, err
	}
	for _, vm := range managed {
		if vm.MachineID == machineID {
			return vm, true, nil
		}
	}
	return vmInstance{}, false, nil
}

// findVM locates any VM (incl. a template) by VMID across the cluster.
func (r *proxmoxReal) findVM(ctx context.Context, vmid int) (vmInstance, error) {
	cluster, err := r.client.Cluster(ctx)
	if err != nil {
		return vmInstance{}, fmt.Errorf("get cluster: %w", err)
	}
	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return vmInstance{}, fmt.Errorf("list cluster VM resources: %w", err)
	}
	for _, res := range resources {
		if res.Type == "qemu" && int(res.VMID) == vmid {
			return vmInstance{Node: res.Node, VMID: vmid, Name: res.Name}, nil
		}
	}
	return vmInstance{}, fmt.Errorf("VMID %d not found in cluster", vmid)
}

// nodeVM opens the go-proxmox VM handle for a (node, vmid).
func (r *proxmoxReal) nodeVM(ctx context.Context, node string, vmid int) (*proxmox.VirtualMachine, error) {
	n, err := r.client.Node(ctx, node)
	if err != nil {
		return nil, err
	}
	return n.VirtualMachine(ctx, vmid)
}

// waitTask waits for a Proxmox task to complete, honouring ctx, and fails on a
// non-OK task exit status.
func (r *proxmoxReal) waitTask(ctx context.Context, task *proxmox.Task) error {
	if task == nil {
		return nil
	}
	// The kit's transition ctx (e.g. the 8m Create budget) is the real bound:
	// Wait pings with ctx every tick, so a cancelled ctx aborts the wait promptly.
	// taskTimeout is only a generous never-terminates backstop and MUST exceed the
	// longest transition budget so it never fires before ctx — using the short
	// per-exec timeout here would prematurely fail a slow-storage clone that is
	// still within the Create budget.
	if err := task.Wait(ctx, 2*time.Second, r.taskTimeout); err != nil {
		return err
	}
	if task.IsFailed || (task.ExitStatus != "" && task.ExitStatus != "OK") {
		return fmt.Errorf("task %s failed: %s", task.UPID, task.ExitStatus)
	}
	return nil
}

// isManagedVM reports whether a VM (by its tag list and name) is one this
// provider manages. It accepts EITHER the marker tag OR the name prefix: the tag
// is applied only after the clone task completes, so a cloned-but-not-yet-tagged
// VM is recognised by its name (set atomically in the clone request), which is
// what stops a retried Create from cloning a duplicate and leaking the first
// VM's disks.
func isManagedVM(tags, name string) bool {
	return hasTag(tags, tagPrefix) || strings.HasPrefix(name, vmNamePrefix)
}

// hasTag reports whether the Proxmox tag list (a ';'/','-separated string)
// contains the given tag.
func hasTag(tags, want string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if t == want {
			return true
		}
	}
	return false
}

// isNotFound reports whether an error is a Proxmox 404 / not-found.
func isNotFound(err error) bool {
	return err != nil && (proxmox.IsNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "does not exist"))
}

// cloneName derives a DNS-safe VM name from the machine id + instance type. PVE
// VM names must match a hostname pattern ([A-Za-z0-9-], dots allowed), so the
// machine id (which carries '/' and '.') is sanitized.
func cloneName(machineID, instanceType string) string {
	raw := vmNamePrefix + sanitizeName(instanceType) + "-" + sanitizeName(machineID)
	if len(raw) > 63 {
		raw = raw[:63]
	}
	return strings.Trim(raw, "-")
}

// sanitizeName maps an arbitrary string to the PVE VM-name charset
// ([A-Za-z0-9-]), replacing every other rune with '-'.
func sanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, ch := range s {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

var _ proxmoxClient = (*proxmoxReal)(nil)
