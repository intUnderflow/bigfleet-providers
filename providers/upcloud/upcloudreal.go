package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/service"

	"golang.org/x/crypto/ssh"
)

// BigFleet server-label keys. labelManaged marks our servers so DescribeManaged
// never touches anything else; the rest let inventory and bindings be recovered
// from UpCloud alone. The (possibly slash-bearing) machine id is base32-encoded
// into its label value and decoded back on read.
const (
	labelManaged   = "bigfleet-managed"
	labelMachineID = "bigfleet-machine-id"
	labelCluster   = "bigfleet-cluster"
)

// UpCloud server states (the API reports these as plain strings).
const (
	stateStarted = "started"
	stateStopped = "stopped"
)

// upcloudRealConfig is the launch configuration for the production UpCloud
// client.
type upcloudRealConfig struct {
	Username string
	Password string
	Zone     string // UpCloud zone id this process serves (e.g. fi-hel1)
	Template string // OS template storage UUID to clone at CreateServer

	// SSHSigner authenticates the SSH session used by ApplyBootstrap / DrainNode.
	// Nil disables SSH delivery (Configure cannot deliver the bootstrap blob).
	SSHSigner ssh.Signer
	SSHUser   string
	// SSHPublicKey is the operator/admin authorized key injected into the server
	// at create (LoginUser.SSHKeys), so the SSHSigner can later authenticate.
	SSHPublicKey string
	// BootstrapHookPath is the executable on the base image that consumes the
	// delivered bootstrap blob (written to <path>.blob) and joins the cluster.
	BootstrapHookPath string

	// StateWaitTimeout caps how long a power transition waits to reach its desired
	// state (the kit's per-transition timeout, carried on ctx, usually fires
	// first).
	StateWaitTimeout time.Duration
	// StorageSizeGB is the cloned OS-template storage size, in gigabytes.
	StorageSizeGB int
}

func (c *upcloudRealConfig) withDefaults() {
	if c.SSHUser == "" {
		c.SSHUser = "root"
	}
	if c.BootstrapHookPath == "" {
		c.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	if c.StateWaitTimeout <= 0 {
		c.StateWaitTimeout = 10 * time.Minute
	}
	if c.StorageSizeGB <= 0 {
		c.StorageSizeGB = 25
	}
}

// upcloudService is the subset of upcloud-go-api's service.Service the real
// client uses. Declaring it as an interface keeps the SDK at the edge and makes
// the client unit-testable without a live account.
type upcloudService interface {
	CreateServer(ctx context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error)
	GetServerDetails(ctx context.Context, r *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error)
	GetServersWithFilters(ctx context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error)
	StartServer(ctx context.Context, r *request.StartServerRequest) (*upcloud.ServerDetails, error)
	StopServer(ctx context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error)
	WaitForServerState(ctx context.Context, r *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error)
	DeleteServerAndStorages(ctx context.Context, r *request.DeleteServerAndStoragesRequest) error
	ModifyServer(ctx context.Context, r *request.ModifyServerRequest) (*upcloud.ServerDetails, error)
	GetPlans(ctx context.Context) (*upcloud.Plans, error)
}

// upcloudReal is the production upcloudClient, backed by upcloud-go-api. Inventory
// and bindings are recovered from server labels; the cluster-specific bootstrap
// and the drain are delivered over SSH with a host key verified against the
// fingerprint pinned at Create.
type upcloudReal struct {
	cfg upcloudRealConfig
	svc upcloudService
	log *slog.Logger
}

func newUpcloudReal(cfg upcloudRealConfig, logger *slog.Logger) (*upcloudReal, error) {
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("upcloud: UPCLOUD_USERNAME and UPCLOUD_PASSWORD (an API sub-account) are required for the upcloud backend")
	}
	if cfg.Zone == "" {
		return nil, errors.New("upcloud: --zone is required for the upcloud backend")
	}
	if cfg.Template == "" {
		return nil, errors.New("upcloud: --template (an OS template storage UUID) is required for the upcloud backend")
	}
	cfg.withDefaults()
	c := client.New(cfg.Username, cfg.Password, client.WithTimeout(30*time.Second))
	return &upcloudReal{cfg: cfg, svc: service.New(c), log: logger}, nil
}

func (r *upcloudReal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	// Pre-create idempotency check (§4.5). UpCloud has NO client idempotency token
	// and enforces NO title/hostname uniqueness, so the substrate offers nothing to
	// dedup on — a retried or restart-re-driven Create would otherwise launch a
	// SECOND server under the same bigfleet-machine-id label, leaking a billed,
	// Delete-unreachable orphan. So look up a managed server for this machine id
	// FIRST and adopt it if one already exists, instead of provisioning a duplicate.
	if existing := r.serverByMachineID(ctx, spec.MachineID); existing != nil {
		return r.waitStarted(ctx, existing.UUID)
	}

	// Mint an SSH host key for the server and inject it via cloud-init, so the host
	// boots presenting a key we already know. Its fingerprint is pinned in a label
	// and verified on every later Configure/Drain SSH connection — closing the MITM
	// window on the (secret-bearing) bootstrap delivery.
	hostKey, err := generateHostKey()
	if err != nil {
		return serverInstance{}, err
	}
	userData, err := buildUserData(spec.BaseUserData, hostKey.cloudConfig())
	if err != nil {
		return serverInstance{}, fmt.Errorf("assemble user-data: %w", err)
	}

	title := serverTitle(spec)
	var sshKeys []string
	if r.cfg.SSHPublicKey != "" {
		sshKeys = append(sshKeys, r.cfg.SSHPublicKey)
	}
	req := &request.CreateServerRequest{
		Zone:     spec.Zone,
		Title:    title,
		Hostname: hostnameFor(spec),
		Plan:     spec.Plan,
		Metadata: upcloud.True, // metadata service on (cloud-init)
		UserData: userData,
		LoginUser: &request.LoginUser{
			Username: r.cfg.SSHUser,
			SSHKeys:  sshKeys,
		},
		StorageDevices: request.CreateServerStorageDeviceSlice{
			{
				Action:  "clone",
				Storage: spec.Template,
				Title:   title + "-osdisk",
				Size:    r.cfg.StorageSizeGB,
				Tier:    "maxiops",
			},
		},
		Networking: &request.CreateServerNetworking{
			Interfaces: request.CreateServerInterfaceSlice{
				{
					IPAddresses: request.CreateServerIPAddressSlice{
						{Family: upcloud.IPAddressFamilyIPv4},
					},
					Type: upcloud.IPAddressAccessPublic,
				},
				{
					IPAddresses: request.CreateServerIPAddressSlice{
						{Family: upcloud.IPAddressFamilyIPv4},
					},
					Type: upcloud.IPAddressAccessUtility,
				},
			},
		},
		Labels: &upcloud.LabelSlice{
			{Key: labelManaged, Value: "true"},
			{Key: labelMachineID, Value: encodeMachineID(spec.MachineID)},
			{Key: labelHostKeyFP, Value: hostKey.fingerprint},
		},
	}

	details, err := r.svc.CreateServer(ctx, req)
	if err != nil {
		// CreateServer returned an error, but the server may have been created
		// before the response was lost (a transport blip after creation). Recover an
		// already-created server for this machine id instead of failing/duplicating.
		if existing := r.serverByMachineID(ctx, spec.MachineID); existing != nil {
			return r.waitStarted(ctx, existing.UUID)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.Plan, err)
	}
	if details == nil || details.UUID == "" {
		return serverInstance{}, fmt.Errorf("create server %s: empty result", spec.Plan)
	}
	return r.waitStarted(ctx, details.UUID)
}

// waitForState blocks until the server reaches desired, bounded by
// StateWaitTimeout (the backend's worst-case cap) layered under the caller's ctx
// (the kit's per-transition timeout). WaitForServerState honours ctx cancellation.
func (r *upcloudReal) waitForState(ctx context.Context, uuid, desired string) (*upcloud.ServerDetails, error) {
	if r.cfg.StateWaitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.StateWaitTimeout)
		defer cancel()
	}
	return r.svc.WaitForServerState(ctx, &request.WaitForServerStateRequest{UUID: uuid, DesiredState: desired})
}

// waitStarted blocks until the server reaches 'started' (so the kit's IDLE means
// "reachable host" and the immediately-following Configure does not race a
// still-initializing server).
func (r *upcloudReal) waitStarted(ctx context.Context, uuid string) (serverInstance, error) {
	details, err := r.waitForState(ctx, uuid, stateStarted)
	if err != nil {
		return serverInstance{}, fmt.Errorf("wait for server %s to start: %w", uuid, err)
	}
	return r.toServerInstance(details), nil
}

// DeleteServer stops the server (UpCloud refuses to delete a running one) and
// deletes it together with its attached storage. Idempotent: an already-gone
// server is success.
func (r *upcloudReal) DeleteServer(ctx context.Context, uuid string) error {
	details, err := r.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: uuid})
	if err != nil {
		if isNotFound(err) {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("delete: lookup server %s: %w", uuid, err)
	}
	if details.State != stateStopped {
		if _, err := r.svc.StopServer(ctx, &request.StopServerRequest{
			UUID:     uuid,
			StopType: "hard",
			Timeout:  2 * time.Minute,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete: stop server %s: %w", uuid, err)
		}
		if _, err := r.waitForState(ctx, uuid, stateStopped); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete: wait for server %s to stop: %w", uuid, err)
		}
	}
	// Delete the server AND its attached storage in one shot — UpCloud storage is a
	// SEPARATE billable resource, so deleting only the server leaks the disk.
	if err := r.svc.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{
		UUID:    uuid,
		Backups: request.DeleteStorageBackupsModeDelete,
	}); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete server and storages %s: %w", uuid, err)
	}
	return nil
}

func (r *upcloudReal) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	servers, err := r.svc.GetServersWithFilters(ctx, &request.GetServersWithFiltersRequest{
		Filters: []request.QueryFilter{
			request.FilterLabelKey{Key: labelManaged},
		},
	})
	if err != nil {
		return nil, err
	}
	out := make([]serverInstance, 0, len(servers.Servers))
	for i := range servers.Servers {
		// The list endpoint omits labels and IPs, so fetch details for each of our
		// (label-filtered) servers to recover the machine-id binding and address.
		details, derr := r.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: servers.Servers[i].UUID})
		if derr != nil {
			if isNotFound(derr) {
				continue
			}
			return nil, fmt.Errorf("describe: server %s details: %w", servers.Servers[i].UUID, derr)
		}
		out = append(out, r.toServerInstance(details))
	}
	return out, nil
}

// EnsureRunning powers a stopped server back on and waits for 'started'.
func (r *upcloudReal) EnsureRunning(ctx context.Context, srv serverInstance) (serverInstance, error) {
	details, err := r.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: srv.UUID})
	if err != nil {
		return srv, fmt.Errorf("look up server %s: %w", srv.UUID, err)
	}
	if details.State == stateStarted {
		return r.toServerInstance(details), nil
	}
	if _, err := r.svc.StartServer(ctx, &request.StartServerRequest{UUID: srv.UUID}); err != nil {
		return srv, fmt.Errorf("start server %s: %w", srv.UUID, err)
	}
	return r.waitStarted(ctx, srv.UUID)
}

func (r *upcloudReal) ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
	if r.cfg.SSHSigner == nil {
		return fmt.Errorf("configure: SSH delivery disabled (set --ssh-key); cannot deliver bootstrap to %s", srv.UUID)
	}
	srv, err := r.ensureIPv4(ctx, srv)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	// Deliver the opaque bootstrap blob to the node and run the base image's hook.
	// The image must ship the hook at BootstrapHookPath; it receives the blob at
	// <hook>.blob and joins the cluster. We wait for it to SUCCEED, so a failed
	// bootstrap surfaces as FAILED.
	blob := base64.StdEncoding.EncodeToString(bootstrap) // base64 -d is universally available
	hook := shellQuote(r.cfg.BootstrapHookPath)
	blobPath := shellQuote(r.cfg.BootstrapHookPath + ".blob")
	script := fmt.Sprintf(
		"set -euo pipefail; umask 077; mkdir -p \"$(dirname %s)\"; echo %s | base64 -d > %s; %s %s",
		blobPath, shellQuote(blob), blobPath, hook, shellQuote(clusterID))
	if err := r.runSSH(ctx, srv, script); err != nil {
		return err
	}
	// Record the binding label only AFTER the bootstrap actually succeeded, so a
	// failed Configure never leaves a server mislabelled as bound to a cluster it
	// never joined.
	return r.setLabel(ctx, srv.UUID, labelCluster, clusterID)
}

func (r *upcloudReal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	if r.cfg.SSHSigner == nil {
		// No SSH path: at least remove the binding label so the machine returns to an
		// unbound state in inventory.
		return r.clearLabel(ctx, srv.UUID, labelCluster)
	}
	srv, err := r.ensureIPv4(ctx, srv)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	grace := gracePeriodSeconds
	if grace <= 0 {
		grace = 1
	}
	// cordon tolerates a re-run (|| true); the DRAIN must NOT swallow its failure —
	// an incomplete drain has to surface as FAILED rather than a false Idle.
	script := fmt.Sprintf(
		"set -euo pipefail; node=$(hostname -f 2>/dev/null || hostname); "+
			"kubectl cordon \"$node\" || true; "+
			"kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds",
		grace, grace)
	if err := r.runSSH(ctx, srv, script); err != nil {
		return err
	}
	return r.clearLabel(ctx, srv.UUID, labelCluster)
}

func (r *upcloudReal) DescribePlanCapacities(ctx context.Context, plans []string) (map[string]planCapacity, error) {
	all, err := r.svc.GetPlans(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{}, len(plans))
	for _, p := range plans {
		want[p] = struct{}{}
	}
	out := make(map[string]planCapacity, len(plans))
	for _, p := range all.Plans {
		if _, ok := want[p.Name]; !ok {
			continue
		}
		out[p.Name] = planCapacity{
			Cores:  p.CoreNumber,
			MemMiB: int64(p.MemoryAmount), // UpCloud reports memory in MiB
		}
	}
	return out, nil
}

// --- helpers --------------------------------------------------------------

func (r *upcloudReal) toServerInstance(d *upcloud.ServerDetails) serverInstance {
	out := serverInstance{
		UUID:      d.UUID,
		Plan:      d.Plan,
		Zone:      d.Zone,
		Running:   d.State == stateStarted,
		MachineID: decodeMachineID(labelValue(d.Labels, labelMachineID)),
		ClusterID: labelValue(d.Labels, labelCluster),
		HostKeyFP: labelValue(d.Labels, labelHostKeyFP),
	}
	for _, ip := range d.IPAddresses {
		if ip.Access == upcloud.IPAddressAccessPublic && ip.Family == upcloud.IPAddressFamilyIPv4 {
			out.PublicIPv4 = ip.Address
			break
		}
	}
	return out
}

func (r *upcloudReal) serverByMachineID(ctx context.Context, machineID string) *serverInstance {
	managed, err := r.DescribeManaged(ctx)
	if err != nil {
		return nil
	}
	for i := range managed {
		if managed[i].MachineID == machineID {
			return &managed[i]
		}
	}
	return nil
}

// ensureIPv4 returns srv with a populated PublicIPv4, re-fetching the server by
// UUID when the cached view lacks one (e.g. the minimal fallback view the
// backend's resolveHost builds when a transient describe missed the server).
func (r *upcloudReal) ensureIPv4(ctx context.Context, srv serverInstance) (serverInstance, error) {
	if srv.PublicIPv4 != "" {
		return srv, nil
	}
	details, err := r.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: srv.UUID})
	if err != nil {
		return srv, fmt.Errorf("look up server %s: %w", srv.UUID, err)
	}
	full := r.toServerInstance(details)
	if full.PublicIPv4 == "" {
		return srv, fmt.Errorf("server %s has no public IPv4 for SSH delivery", srv.UUID)
	}
	// Preserve the pinned fingerprint from the caller's view if details lacked it.
	if full.HostKeyFP == "" {
		full.HostKeyFP = srv.HostKeyFP
	}
	return full, nil
}

func (r *upcloudReal) setLabel(ctx context.Context, uuid, key, value string) error {
	return r.updateLabels(ctx, uuid, func(labels map[string]string) { labels[key] = value })
}

func (r *upcloudReal) clearLabel(ctx context.Context, uuid, key string) error {
	return r.updateLabels(ctx, uuid, func(labels map[string]string) { delete(labels, key) })
}

func (r *upcloudReal) updateLabels(ctx context.Context, uuid string, mutate func(map[string]string)) error {
	details, err := r.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: uuid})
	if err != nil {
		return err
	}
	labels := map[string]string{}
	for _, l := range details.Labels {
		labels[l.Key] = l.Value
	}
	mutate(labels)
	slice := upcloud.LabelSlice{}
	for k, v := range labels {
		slice = append(slice, upcloud.Label{Key: k, Value: v})
	}
	_, err = r.svc.ModifyServer(ctx, &request.ModifyServerRequest{UUID: uuid, Labels: &slice})
	return err
}

// runSSH dials the server, runs script over a single session, and returns an
// error unless it exits 0. The server's SSH host key is verified against the
// fingerprint pinned at Create (srv.HostKeyFP); a mismatch aborts the connection
// as a possible MITM. For a server with no pin (an orphan, or one created before
// host-key pinning) it trust-on-first-uses and persists the observed key, so all
// later connections are verified.
func (r *upcloudReal) runSSH(ctx context.Context, srv serverInstance, script string) error {
	host := srv.PublicIPv4
	if host == "" {
		return fmt.Errorf("ssh: no public IPv4 for server %s", srv.UUID)
	}
	var tofuFP string
	cfg := &ssh.ClientConfig{
		User:            r.cfg.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(r.cfg.SSHSigner)},
		HostKeyCallback: hostKeyCallback(srv.HostKeyFP, func(fp string) { tofuFP = fp }),
		Timeout:         15 * time.Second,
	}
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "22"))
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", host, err)
	}
	defer func() { _ = conn.Close() }()
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(host, "22"), cfg)
	if err != nil {
		return fmt.Errorf("ssh handshake %s: %w", host, err)
	}
	// Handshake passed verification. If this was a trust-on-first-use (no prior
	// pin), persist the observed fingerprint so every later connection is checked.
	if srv.HostKeyFP == "" && tofuFP != "" {
		if r.log != nil {
			r.log.Warn("pinning SSH host key on first use (no pre-injected key)", "server", srv.UUID)
		}
		if err := r.setLabel(ctx, srv.UUID, labelHostKeyFP, tofuFP); err != nil && r.log != nil {
			r.log.Warn("failed to persist TOFU host-key pin", "server", srv.UUID, "err", err)
		}
	}
	cl := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = cl.Close() }()

	session, err := cl.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session %s: %w", host, err)
	}
	defer func() { _ = session.Close() }()

	done := make(chan error, 1)
	go func() { done <- session.Run(script) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return fmt.Errorf("ssh command on %s did not complete: %w", host, ctx.Err())
	case err := <-done:
		if err != nil {
			return fmt.Errorf("ssh command on %s failed: %w", host, err)
		}
		return nil
	}
}

func labelValue(labels upcloud.LabelSlice, key string) string {
	for _, l := range labels {
		if l.Key == key {
			return l.Value
		}
	}
	return ""
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var prob *upcloud.Problem
	if errors.As(err, &prob) {
		return prob.Status == 404
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

// serverTitle derives a stable UpCloud title from the operation id (stable
// across a retried Create), so a transport retry maps to the same server.
func serverTitle(spec serverSpec) string {
	token := spec.IdempotencyToken
	if token == "" {
		token = spec.MachineID
	}
	title := "bigfleet-" + strings.ToLower(machineIDEncoding.EncodeToString([]byte(token)))
	if len(title) > 64 {
		title = title[:64]
	}
	return title
}

// hostnameFor derives a DNS-safe hostname for the server.
func hostnameFor(spec serverSpec) string {
	h := "bigfleet-" + strings.ToLower(machineIDEncoding.EncodeToString([]byte(spec.MachineID)))
	if len(h) > 63 {
		h = h[:63]
	}
	return h
}

// shellQuote single-quotes a string for safe interpolation into a /bin/sh
// command (the blob and cluster id are opaque, so never trust their bytes).
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}

var _ upcloudClient = (*upcloudReal)(nil)
