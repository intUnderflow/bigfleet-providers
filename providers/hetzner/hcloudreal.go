package main

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"golang.org/x/crypto/ssh"
)

// BigFleet server-label keys. bigfleet-managed marks our servers so
// DescribeManaged never touches anything else; the rest let inventory and
// bindings be recovered from Hetzner alone. Hetzner label VALUES are constrained
// (max 63 chars, [a-zA-Z0-9._-]), so the (possibly slash-bearing) machine id is
// base32-encoded into its value and decoded back on read.
const (
	labelManaged   = "bigfleet-managed"
	labelMachineID = "bigfleet-machine-id"
	labelCluster   = "bigfleet-cluster"
)

// machineIDEncoding encodes the machine id into a Hetzner-label-safe value
// (lowercase base32 without padding → only [a-z2-7], well within the value
// charset). Round-trips any machine id up to ~39 bytes within the 63-char label
// limit; longer ids rely on the FileStore for restart recovery (the documented
// primary path).
var machineIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func encodeMachineID(id string) string {
	return strings.ToLower(machineIDEncoding.EncodeToString([]byte(id)))
}

func decodeMachineID(label string) string {
	b, err := machineIDEncoding.DecodeString(strings.ToUpper(label))
	if err != nil {
		return ""
	}
	return string(b)
}

// hcloudRealConfig is the launch configuration for the production Hetzner Cloud
// client.
type hcloudRealConfig struct {
	Token    string
	Image    string // base image name/id for Server.Create
	Location string // default location label (informational)
	EURtoUSD float64

	// SSHSigner authenticates the SSH session used by ApplyBootstrap / DrainNode
	// (Hetzner Cloud has no agent/command API). Nil disables SSH delivery.
	SSHSigner ssh.Signer
	SSHUser   string
	// BootstrapHookPath is the executable on the base image that consumes the
	// delivered bootstrap blob (written to <path>.blob) and joins the cluster.
	BootstrapHookPath string

	// CreateWaitTimeout caps how long CreateServer waits for the server to reach
	// 'running' (the kit's Create timeout, carried on ctx, usually fires first).
	CreateWaitTimeout time.Duration
	// PollInterval is how often CreateServer polls the server status.
	PollInterval time.Duration
}

func (c *hcloudRealConfig) withDefaults() {
	if c.EURtoUSD <= 0 {
		c.EURtoUSD = defaultEURtoUSD
	}
	if c.SSHUser == "" {
		c.SSHUser = "root"
	}
	if c.BootstrapHookPath == "" {
		c.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 10 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
}

// hcloudReal is the production hcloudClient, backed by hcloud-go. Inventory and
// bindings are recovered from server labels; the cluster-specific bootstrap and
// the drain are delivered over SSH (Hetzner Cloud exposes no in-guest command
// API), so the base image must authorise --ssh-key.
type hcloudReal struct {
	cfg    hcloudRealConfig
	client *hcloud.Client
	logger *slog.Logger
}

func newHCloudReal(cfg hcloudRealConfig, logger *slog.Logger) (*hcloudReal, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("hcloud: token is required for the hetzner backend")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("hcloud: --image is required for the hetzner backend")
	}
	cfg.withDefaults()
	return &hcloudReal{
		cfg:    cfg,
		client: hcloud.NewClient(hcloud.WithToken(cfg.Token)),
		logger: logger,
	}, nil
}

func (r *hcloudReal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	serverType, _, err := r.client.ServerType.GetByName(ctx, spec.ServerType)
	if err != nil {
		return serverInstance{}, fmt.Errorf("resolve server type %s: %w", spec.ServerType, err)
	}
	if serverType == nil {
		return serverInstance{}, fmt.Errorf("unknown server type %q", spec.ServerType)
	}
	image, _, err := r.client.Image.GetForArchitecture(ctx, spec.Image, serverType.Architecture)
	if err != nil {
		return serverInstance{}, fmt.Errorf("resolve image %s: %w", spec.Image, err)
	}
	if image == nil {
		return serverInstance{}, fmt.Errorf("unknown image %q for architecture %s", spec.Image, serverType.Architecture)
	}
	location, _, err := r.client.Location.GetByName(ctx, spec.Location)
	if err != nil {
		return serverInstance{}, fmt.Errorf("resolve location %s: %w", spec.Location, err)
	}
	if location == nil {
		// GetByName returns (nil, nil) for an unknown location; reject it with a
		// clear message rather than passing a nil Location (which would silently
		// fall back to the project default, mis-placing the server's zone).
		return serverInstance{}, fmt.Errorf("unknown location %q", spec.Location)
	}

	// Mint an SSH host key for the server and inject it via cloud-init, so the
	// host boots presenting a key we already know. Its fingerprint is pinned in a
	// label and verified on every later Configure/Drain SSH connection — closing
	// the MITM window on the (secret-bearing) bootstrap delivery.
	hostKey, err := generateHostKey()
	if err != nil {
		return serverInstance{}, err
	}
	userData, err := buildUserData(spec.BaseUserData, hostKey.cloudConfig())
	if err != nil {
		return serverInstance{}, fmt.Errorf("assemble user-data: %w", err)
	}

	opts := hcloud.ServerCreateOpts{
		// The operation id (idempotency token) makes the name stable across a
		// retried Create, so a transport retry maps to the same server name.
		Name:       serverName(spec),
		ServerType: serverType,
		Image:      image,
		Location:   location,
		UserData:   userData,
		Labels: map[string]string{
			labelManaged:   "true",
			labelMachineID: encodeMachineID(spec.MachineID),
			labelHostKeyFP: hostKey.fingerprint,
		},
	}
	res, _, err := r.client.Server.Create(ctx, opts)
	if err != nil {
		// A retried Create whose name already exists is the idempotent case:
		// recover the existing server instead of failing.
		if existing := r.serverByName(ctx, opts.Name); existing != nil {
			return r.waitRunning(ctx, existing.ID)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.ServerType, err)
	}
	if res.Server == nil || res.Server.ID == 0 {
		return serverInstance{}, fmt.Errorf("create server %s: empty result", spec.ServerType)
	}
	return r.waitRunning(ctx, res.Server.ID)
}

// waitRunning polls until the server reaches the running status (so the kit's
// IDLE means "reachable host" and the immediately-following Configure does not
// race a still-initializing server). ctx (the kit's Create timeout) cancels it.
func (r *hcloudReal) waitRunning(ctx context.Context, id int64) (serverInstance, error) {
	deadline := time.Now().Add(r.cfg.CreateWaitTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		srv, _, err := r.client.Server.GetByID(ctx, id)
		if err != nil {
			return serverInstance{}, fmt.Errorf("poll server %d: %w", id, err)
		}
		if srv == nil {
			return serverInstance{}, fmt.Errorf("server %d vanished while creating", id)
		}
		if srv.Status == hcloud.ServerStatusRunning {
			return r.toServerInstance(srv), nil
		}
		select {
		case <-ctx.Done():
			return serverInstance{}, fmt.Errorf("waiting for server %d to run: %w", id, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return serverInstance{}, fmt.Errorf("server %d did not reach running within %s", id, r.cfg.CreateWaitTimeout)
			}
		}
	}
}

func (r *hcloudReal) DeleteServer(ctx context.Context, serverID string) error {
	id, err := strconv.ParseInt(serverID, 10, 64)
	if err != nil {
		return fmt.Errorf("delete: bad server id %q: %w", serverID, err)
	}
	srv, _, err := r.client.Server.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("delete: lookup server %d: %w", id, err)
	}
	if srv == nil {
		return nil // already gone — idempotent
	}
	_, _, err = r.client.Server.DeleteWithResult(ctx, srv)
	return err
}

func (r *hcloudReal) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	servers, err := r.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: labelManaged + "=true"},
	})
	if err != nil {
		return nil, err
	}
	out := make([]serverInstance, 0, len(servers))
	for _, srv := range servers {
		out = append(out, r.toServerInstance(srv))
	}
	return out, nil
}

func (r *hcloudReal) ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
	if r.cfg.SSHSigner == nil {
		return fmt.Errorf("configure: SSH delivery disabled (set --ssh-key); cannot deliver bootstrap to %s", srv.ServerID)
	}
	srv, err := r.ensureIPv4(ctx, srv)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	// Deliver the opaque bootstrap blob to the node and run the base image's
	// hook. The image must ship the hook at BootstrapHookPath; it receives the
	// blob at <hook>.blob and joins the cluster. We wait for it to SUCCEED, so a
	// failed bootstrap surfaces as FAILED.
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
	// failed Configure (SSH disabled / unreachable / hook non-zero) never leaves a
	// server mislabelled as bound to a cluster it never joined.
	if err := r.setLabel(ctx, srv.ServerID, labelCluster, clusterID); err != nil {
		return fmt.Errorf("label cluster binding: %w", err)
	}
	return nil
}

func (r *hcloudReal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	if r.cfg.SSHSigner == nil {
		// No SSH path: at least remove the binding label so the machine returns
		// to an unbound state in inventory.
		return r.clearLabel(ctx, srv.ServerID, labelCluster)
	}
	srv, err := r.ensureIPv4(ctx, srv)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	grace := gracePeriodSeconds
	if grace <= 0 {
		grace = 1
	}
	// cordon tolerates a re-run (|| true); the DRAIN must NOT swallow its failure
	// — an incomplete drain has to surface as FAILED rather than a false Idle.
	script := fmt.Sprintf(
		"set -euo pipefail; node=$(hostname -f 2>/dev/null || hostname); "+
			"kubectl cordon \"$node\" || true; "+
			"kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds",
		grace, grace)
	if err := r.runSSH(ctx, srv, script); err != nil {
		return err
	}
	return r.clearLabel(ctx, srv.ServerID, labelCluster)
}

// ensureIPv4 returns srv with a populated PublicIPv4, re-fetching the server by
// id when the cached view lacks one — e.g. the minimal fallback view the
// backend's resolveHost builds when a transient DescribeManaged missed the
// server. SSH-based Configure/Drain need the address, so this avoids a
// misleading "no public IPv4" error when the server is in fact reachable.
func (r *hcloudReal) ensureIPv4(ctx context.Context, srv serverInstance) (serverInstance, error) {
	if srv.PublicIPv4 != "" {
		return srv, nil
	}
	id, err := strconv.ParseInt(srv.ServerID, 10, 64)
	if err != nil {
		return srv, fmt.Errorf("bad server id %q: %w", srv.ServerID, err)
	}
	fresh, _, err := r.client.Server.GetByID(ctx, id)
	if err != nil {
		return srv, fmt.Errorf("look up server %s: %w", srv.ServerID, err)
	}
	if fresh == nil {
		return srv, fmt.Errorf("server %s not found", srv.ServerID)
	}
	full := r.toServerInstance(fresh)
	if full.PublicIPv4 == "" {
		return srv, fmt.Errorf("server %s has no public IPv4 for SSH delivery", srv.ServerID)
	}
	return full, nil
}

func (r *hcloudReal) PriceUSD(ctx context.Context, serverType, location string) (float64, error) {
	st, _, err := r.client.ServerType.GetByName(ctx, serverType)
	if err != nil {
		return 0, err
	}
	if st == nil {
		return 0, fmt.Errorf("unknown server type %q", serverType)
	}
	for _, p := range st.Pricings {
		if p.Location != nil && p.Location.Name == location {
			eur, perr := strconv.ParseFloat(p.Hourly.Gross, 64)
			if perr != nil {
				return 0, fmt.Errorf("parse hourly price for %s/%s: %w", serverType, location, perr)
			}
			return eur * r.cfg.EURtoUSD, nil
		}
	}
	return 0, fmt.Errorf("no pricing for %s in %s", serverType, location)
}

func (r *hcloudReal) DescribeServerTypeCapacities(ctx context.Context, serverTypes []string) (map[string]serverCapacity, error) {
	out := make(map[string]serverCapacity, len(serverTypes))
	for _, name := range serverTypes {
		st, _, err := r.client.ServerType.GetByName(ctx, name)
		if err != nil {
			return nil, err
		}
		if st == nil {
			continue // omitted; caller falls back to the pinned table
		}
		out[name] = serverCapacity{
			VCPU:   st.Cores,
			MemMiB: int64(st.Memory * 1024), // Memory is GiB (float32)
		}
	}
	return out, nil
}

// --- helpers --------------------------------------------------------------

func (r *hcloudReal) toServerInstance(srv *hcloud.Server) serverInstance {
	out := serverInstance{
		ServerID:  strconv.FormatInt(srv.ID, 10),
		MachineID: decodeMachineID(srv.Labels[labelMachineID]),
		ClusterID: srv.Labels[labelCluster],
		HostKeyFP: srv.Labels[labelHostKeyFP],
		Running: srv.Status == hcloud.ServerStatusRunning ||
			srv.Status == hcloud.ServerStatusInitializing ||
			srv.Status == hcloud.ServerStatusStarting,
	}
	if srv.ServerType != nil {
		out.ServerType = srv.ServerType.Name
	}
	if srv.Location != nil {
		out.Location = srv.Location.Name
	}
	if !srv.PublicNet.IPv4.IsUnspecified() {
		out.PublicIPv4 = srv.PublicNet.IPv4.IP.String()
	}
	return out
}

func (r *hcloudReal) serverByName(ctx context.Context, name string) *hcloud.Server {
	srv, _, err := r.client.Server.GetByName(ctx, name)
	if err != nil {
		return nil
	}
	return srv
}

func (r *hcloudReal) setLabel(ctx context.Context, serverID, key, value string) error {
	return r.updateLabels(ctx, serverID, func(labels map[string]string) {
		labels[key] = value
	})
}

func (r *hcloudReal) clearLabel(ctx context.Context, serverID, key string) error {
	return r.updateLabels(ctx, serverID, func(labels map[string]string) {
		delete(labels, key)
	})
}

func (r *hcloudReal) updateLabels(ctx context.Context, serverID string, mutate func(map[string]string)) error {
	id, err := strconv.ParseInt(serverID, 10, 64)
	if err != nil {
		return fmt.Errorf("bad server id %q: %w", serverID, err)
	}
	srv, _, err := r.client.Server.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if srv == nil {
		return fmt.Errorf("server %s not found", serverID)
	}
	labels := map[string]string{}
	for k, v := range srv.Labels {
		labels[k] = v
	}
	mutate(labels)
	_, _, err = r.client.Server.Update(ctx, srv, hcloud.ServerUpdateOpts{Labels: labels})
	return err
}

// runSSH dials the server, runs script over a single session, and returns an
// error unless it exits 0. The server's SSH host key is verified against the
// fingerprint pinned at Create (srv.HostKeyFP); a mismatch aborts the connection
// as a possible MITM. For a server with no pin (an orphan, or one created before
// host-key pinning) it trust-on-first-uses and persists the observed key, so all
// later connections are verified.
func (r *hcloudReal) runSSH(ctx context.Context, srv serverInstance, script string) error {
	host := srv.PublicIPv4
	if host == "" {
		return fmt.Errorf("ssh: no public IPv4 for server %s", srv.ServerID)
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
		if r.logger != nil {
			r.logger.Warn("pinning SSH host key on first use (no pre-injected key)", "server", srv.ServerID)
		}
		if err := r.setLabel(ctx, srv.ServerID, labelHostKeyFP, tofuFP); err != nil && r.logger != nil {
			r.logger.Warn("failed to persist TOFU host-key pin", "server", srv.ServerID, "err", err)
		}
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
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

// serverName derives a stable, DNS-safe Hetzner server name from the operation
// id (stable across a retried Create), so a transport retry recreates under the
// same name and the create is idempotent.
func serverName(spec serverSpec) string {
	token := spec.IdempotencyToken
	if token == "" {
		token = spec.MachineID
	}
	name := "bigfleet-" + strings.ToLower(machineIDEncoding.EncodeToString([]byte(token)))
	if len(name) > 63 {
		name = name[:63]
	}
	return name
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

var _ hcloudClient = (*hcloudReal)(nil)
