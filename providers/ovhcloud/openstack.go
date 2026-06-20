package main

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"

	"golang.org/x/crypto/ssh"
)

// BigFleet server-metadata keys. metaManaged marks our servers so
// DescribeManaged never touches anything else; the rest let inventory and
// bindings be recovered from OpenStack alone. Nova metadata values allow up to
// 255 bytes of arbitrary text, so the (slash-bearing) machine id is stored
// verbatim — no encoding needed (unlike Hetzner's constrained label values).
const (
	metaManaged   = "bigfleet-managed"
	metaMachineID = "bigfleet-machine-id"
	metaCluster   = "bigfleet-cluster"
)

// nameEncoding renders the operation id into a DNS-safe server name suffix.
var nameEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// uuidRe matches an OpenStack UUID, so a --network value can be used directly as
// an id rather than resolved by name.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ovhRealConfig is the launch configuration for the production OVH Public Cloud
// (OpenStack) client. The Keystone credentials are read from the standard OS_*
// environment (OS_AUTH_URL / OS_USERNAME / OS_PASSWORD / project id /
// OS_USER_DOMAIN_NAME, …) via gophercloud's AuthOptionsFromEnv — never passed on
// the command line — so they arrive from a mounted Secret.
type ovhRealConfig struct {
	Region  string // OpenStack region (GRA, SBG, BHS, …); selects the service endpoint
	Image   string // base image id (UUID) for server create
	KeyName string // OpenStack keypair name injected for SSH access
	Network string // network name or UUID to attach (empty = project default)

	// SSHSigner authenticates the SSH session used by ApplyBootstrap / DrainNode
	// (the on-host bootstrap delivery channel). Nil disables SSH delivery.
	SSHSigner ssh.Signer
	SSHUser   string
	// BootstrapHookPath is the executable on the base image that consumes the
	// delivered bootstrap blob (written to <path>.blob) and joins the cluster.
	BootstrapHookPath string

	// CreateWaitTimeout caps how long CreateServer waits for the server to reach
	// ACTIVE (the kit's Create timeout, carried on ctx, usually fires first).
	CreateWaitTimeout time.Duration
	// PollInterval is how often CreateServer polls the server status.
	PollInterval time.Duration
}

func (c *ovhRealConfig) withDefaults() {
	if c.SSHUser == "" {
		c.SSHUser = "ubuntu"
	}
	if c.BootstrapHookPath == "" {
		c.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 10 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
}

// ovhReal is the production ovhClient, backed by gophercloud/v2. Inventory and
// bindings are recovered from server metadata; the cluster-specific bootstrap
// and the drain are delivered over SSH (the secret-bearing blob is fetched/
// applied on the already-running host), with the host's SSH key pinned at Create
// and verified on every connection.
type ovhReal struct {
	cfg     ovhRealConfig
	compute *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
	logger  *slog.Logger

	mu          sync.Mutex
	flavorIDs   map[string]string // flavor name -> id (cached)
	networkUUID string            // resolved once from cfg.Network
}

func newOVHReal(ctx context.Context, cfg ovhRealConfig, logger *slog.Logger) (*ovhReal, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("openstack: --region is required for the ovh backend")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("openstack: --image (base image id) is required for the ovh backend")
	}
	cfg.withDefaults()

	ao, err := openstack.AuthOptionsFromEnv()
	if err != nil {
		return nil, fmt.Errorf("openstack: read OS_* credentials from environment: %w", err)
	}
	ao.AllowReauth = true
	provider, err := openstack.AuthenticatedClient(ctx, ao)
	if err != nil {
		return nil, fmt.Errorf("openstack: authenticate (Keystone): %w", err)
	}
	eo := gophercloud.EndpointOpts{Region: cfg.Region}
	compute, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("openstack: compute endpoint in region %s: %w", cfg.Region, err)
	}
	r := &ovhReal{
		cfg:       cfg,
		compute:   compute,
		logger:    logger,
		flavorIDs: map[string]string{},
	}
	// The network service is only needed to resolve a network NAME; a UUID is
	// used directly. Build it lazily-but-eagerly so a misconfiguration surfaces
	// at startup rather than on first Create.
	if cfg.Network != "" && !uuidRe.MatchString(cfg.Network) {
		netClient, err := openstack.NewNetworkV2(provider, eo)
		if err != nil {
			return nil, fmt.Errorf("openstack: network endpoint in region %s: %w", cfg.Region, err)
		}
		r.network = netClient
	}
	return r, nil
}

func (r *ovhReal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	flavorID, err := r.flavorID(ctx, spec.Flavor)
	if err != nil {
		return serverInstance{}, err
	}
	netUUID, err := r.resolveNetwork(ctx)
	if err != nil {
		return serverInstance{}, err
	}

	// Mint an SSH host key for the server and inject it via cloud-init, so the
	// host boots presenting a key we already know. Its fingerprint is pinned in
	// metadata and verified on every later Configure/Drain SSH connection —
	// closing the MITM window on the (secret-bearing) bootstrap delivery.
	hostKey, err := generateHostKey()
	if err != nil {
		return serverInstance{}, err
	}
	userData, err := buildUserData(spec.BaseUserData, hostKey.cloudConfig())
	if err != nil {
		return serverInstance{}, fmt.Errorf("assemble user-data: %w", err)
	}

	base := servers.CreateOpts{
		// The operation id (idempotency token) makes the name stable across a
		// retried Create, so a transport retry maps to the same server name.
		Name:      serverName(spec),
		FlavorRef: flavorID,
		ImageRef:  r.cfg.Image,
		UserData:  []byte(userData),
		Metadata: map[string]string{
			metaManaged:   "true",
			metaMachineID: spec.MachineID,
			metaHostKeyFP: hostKey.fingerprint,
		},
	}
	if netUUID != "" {
		base.Networks = []servers.Network{{UUID: netUUID}}
	}
	createOpts := keypairs.CreateOptsExt{CreateOptsBuilder: base, KeyName: r.cfg.KeyName}

	srv, err := servers.Create(ctx, r.compute, createOpts, nil).Extract()
	if err != nil {
		// A retried Create whose name already exists is the idempotent case:
		// recover the existing server instead of failing.
		if existing := r.serverByName(ctx, base.Name); existing != nil {
			return r.waitActive(ctx, existing.ID)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.Flavor, err)
	}
	if srv == nil || srv.ID == "" {
		return serverInstance{}, fmt.Errorf("create server %s: empty result", spec.Flavor)
	}
	return r.waitActive(ctx, srv.ID)
}

// waitActive polls until the server reaches ACTIVE (so the kit's IDLE means
// "reachable host" and the immediately-following Configure does not race a
// still-building server). ctx (the kit's Create timeout) cancels it. An ERROR
// status fails fast.
func (r *ovhReal) waitActive(ctx context.Context, id string) (serverInstance, error) {
	deadline := time.Now().Add(r.cfg.CreateWaitTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		srv, err := servers.Get(ctx, r.compute, id).Extract()
		if err != nil {
			return serverInstance{}, fmt.Errorf("poll server %s: %w", id, err)
		}
		switch strings.ToUpper(srv.Status) {
		case "ACTIVE":
			return r.toServerInstance(srv), nil
		case "ERROR":
			return serverInstance{}, fmt.Errorf("server %s entered ERROR status while creating", id)
		}
		select {
		case <-ctx.Done():
			return serverInstance{}, fmt.Errorf("waiting for server %s to become ACTIVE: %w", id, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return serverInstance{}, fmt.Errorf("server %s did not reach ACTIVE within %s", id, r.cfg.CreateWaitTimeout)
			}
		}
	}
}

func (r *ovhReal) DeleteServer(ctx context.Context, serverID string) error {
	err := servers.Delete(ctx, r.compute, serverID).ExtractErr()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("delete server %s: %w", serverID, err)
	}
	return nil
}

func (r *ovhReal) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	pages, err := servers.List(r.compute, servers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract servers: %w", err)
	}
	out := make([]serverInstance, 0, len(all))
	for i := range all {
		// Nova has no server-side arbitrary-metadata filter, so select ours
		// client-side by the managed marker.
		if all[i].Metadata[metaManaged] != "true" {
			continue
		}
		out = append(out, r.toServerInstance(&all[i]))
	}
	return out, nil
}

func (r *ovhReal) ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
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
		"set -euo pipefail; umask 077; sudo mkdir -p \"$(dirname %s)\"; echo %s | base64 -d | sudo tee %s >/dev/null; sudo %s %s",
		blobPath, shellQuote(blob), blobPath, hook, shellQuote(clusterID))
	if err := r.runSSH(ctx, srv, script); err != nil {
		return err
	}
	// Record the binding metadata only AFTER the bootstrap actually succeeded, so
	// a failed Configure (SSH disabled / unreachable / hook non-zero) never leaves
	// a server mislabelled as bound to a cluster it never joined.
	if err := r.setMetadatum(ctx, srv.ServerID, metaCluster, clusterID); err != nil {
		return fmt.Errorf("record cluster binding: %w", err)
	}
	return nil
}

func (r *ovhReal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	if r.cfg.SSHSigner == nil {
		// No SSH path: at least clear the binding metadata so the machine returns
		// to an unbound state in inventory.
		return r.clearMetadatum(ctx, srv.ServerID, metaCluster)
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
			"sudo kubectl cordon \"$node\" || true; "+
			"sudo kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds",
		grace, grace)
	if err := r.runSSH(ctx, srv, script); err != nil {
		return err
	}
	return r.clearMetadatum(ctx, srv.ServerID, metaCluster)
}

func (r *ovhReal) DescribeFlavorCapacities(ctx context.Context, names []string) (map[string]flavorCapacity, error) {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	pages, err := flavors.ListDetail(r.compute, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list flavors: %w", err)
	}
	all, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return nil, fmt.Errorf("extract flavors: %w", err)
	}
	out := make(map[string]flavorCapacity, len(names))
	for _, fl := range all {
		if _, ok := want[fl.Name]; !ok {
			continue
		}
		r.mu.Lock()
		r.flavorIDs[fl.Name] = fl.ID // warm the id cache as a side effect
		r.mu.Unlock()
		out[fl.Name] = flavorCapacity{VCPU: fl.VCPUs, MemMiB: int64(fl.RAM)} // RAM is MB ≈ MiB
	}
	return out, nil
}

// --- helpers --------------------------------------------------------------

// flavorID resolves a flavor name to its OpenStack id, caching the result.
func (r *ovhReal) flavorID(ctx context.Context, name string) (string, error) {
	r.mu.Lock()
	id, ok := r.flavorIDs[name]
	r.mu.Unlock()
	if ok {
		return id, nil
	}
	pages, err := flavors.ListDetail(r.compute, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("list flavors: %w", err)
	}
	all, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return "", fmt.Errorf("extract flavors: %w", err)
	}
	r.mu.Lock()
	for _, fl := range all {
		r.flavorIDs[fl.Name] = fl.ID
	}
	id, ok = r.flavorIDs[name]
	r.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown flavor %q in region %s", name, r.cfg.Region)
	}
	return id, nil
}

// resolveNetwork returns the network UUID to attach: the configured UUID
// directly, or a name resolved via the networking service (cached).
func (r *ovhReal) resolveNetwork(ctx context.Context) (string, error) {
	if r.cfg.Network == "" {
		return "", nil
	}
	if uuidRe.MatchString(r.cfg.Network) {
		return r.cfg.Network, nil
	}
	r.mu.Lock()
	cached := r.networkUUID
	r.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	pages, err := networks.List(r.network, networks.ListOpts{Name: r.cfg.Network}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("list network %q: %w", r.cfg.Network, err)
	}
	nets, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", fmt.Errorf("extract network %q: %w", r.cfg.Network, err)
	}
	if len(nets) == 0 {
		return "", fmt.Errorf("network %q not found in region %s", r.cfg.Network, r.cfg.Region)
	}
	r.mu.Lock()
	r.networkUUID = nets[0].ID
	r.mu.Unlock()
	return nets[0].ID, nil
}

func (r *ovhReal) toServerInstance(srv *servers.Server) serverInstance {
	out := serverInstance{
		ServerID:   srv.ID,
		MachineID:  srv.Metadata[metaMachineID],
		ClusterID:  srv.Metadata[metaCluster],
		HostKeyFP:  srv.Metadata[metaHostKeyFP],
		Region:     r.cfg.Region,
		Flavor:     flavorName(srv.Flavor),
		PublicIPv4: firstIPv4(srv.Addresses),
		Running:    srv.Status == "ACTIVE" || srv.Status == "BUILD",
	}
	return out
}

func (r *ovhReal) serverByName(ctx context.Context, name string) *servers.Server {
	pages, err := servers.List(r.compute, servers.ListOpts{Name: "^" + regexp.QuoteMeta(name) + "$"}).AllPages(ctx)
	if err != nil {
		return nil
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		return nil
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i]
		}
	}
	return nil
}

// ensureIPv4 returns srv with a populated PublicIPv4, re-fetching the server by
// id when the cached view lacks one — e.g. the minimal fallback view the
// backend's resolveHost builds when a transient DescribeManaged missed the
// server. SSH-based Configure/Drain need the address.
func (r *ovhReal) ensureIPv4(ctx context.Context, srv serverInstance) (serverInstance, error) {
	if srv.PublicIPv4 != "" {
		return srv, nil
	}
	fresh, err := servers.Get(ctx, r.compute, srv.ServerID).Extract()
	if err != nil {
		return srv, fmt.Errorf("look up server %s: %w", srv.ServerID, err)
	}
	full := r.toServerInstance(fresh)
	if full.PublicIPv4 == "" {
		return srv, fmt.Errorf("server %s has no public IPv4 for SSH delivery", srv.ServerID)
	}
	return full, nil
}

func (r *ovhReal) setMetadatum(ctx context.Context, serverID, key, value string) error {
	_, err := servers.CreateMetadatum(ctx, r.compute, serverID, servers.MetadatumOpts{key: value}).Extract()
	if err != nil {
		return fmt.Errorf("set metadata %s on %s: %w", key, serverID, err)
	}
	return nil
}

func (r *ovhReal) clearMetadatum(ctx context.Context, serverID, key string) error {
	err := servers.DeleteMetadatum(ctx, r.compute, serverID, key).ExtractErr()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil // key already absent — idempotent
		}
		return fmt.Errorf("clear metadata %s on %s: %w", key, serverID, err)
	}
	return nil
}

// runSSH dials the server, runs script over a single session, and returns an
// error unless it exits 0. The server's SSH host key is verified against the
// fingerprint pinned at Create (srv.HostKeyFP); a mismatch aborts the connection
// as a possible MITM. For a server with no pin (an orphan, or one created before
// host-key pinning) it trust-on-first-uses and persists the observed key, so all
// later connections are verified.
func (r *ovhReal) runSSH(ctx context.Context, srv serverInstance, script string) error {
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
		if err := r.setMetadatum(ctx, srv.ServerID, metaHostKeyFP, tofuFP); err != nil && r.logger != nil {
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

// serverName derives a stable, DNS-safe server name from the operation id
// (stable across a retried Create), so a transport retry recreates under the
// same name and the create is idempotent.
func serverName(spec serverSpec) string {
	token := spec.IdempotencyToken
	if token == "" {
		token = spec.MachineID
	}
	name := "bigfleet-" + strings.ToLower(nameEncoding.EncodeToString([]byte(token)))
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// flavorName extracts the flavor name from a Nova server's flavor object, which
// (on recent microversions) embeds "original_name".
func flavorName(flavor map[string]any) string {
	if flavor == nil {
		return ""
	}
	if n, ok := flavor["original_name"].(string); ok {
		return n
	}
	// Older microversions expose only the flavor id under "id"; the backend
	// recovers the flavor for pricing/allocatable from the FileStore in that case.
	if id, ok := flavor["id"].(string); ok {
		return id
	}
	return ""
}

// firstIPv4 returns the first IPv4 address across a Nova server's address pools,
// preferring a floating address (publicly reachable) over a fixed one.
func firstIPv4(addresses map[string]any) string {
	var fixed string
	for _, pool := range addresses {
		entries, ok := pool.([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			am, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if v, _ := am["version"].(float64); v != 4 {
				continue
			}
			addr, _ := am["addr"].(string)
			if addr == "" {
				continue
			}
			if t, _ := am["OS-EXT-IPS:type"].(string); t == "floating" {
				return addr
			}
			if fixed == "" {
				fixed = addr
			}
		}
	}
	return fixed
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

var _ ovhClient = (*ovhReal)(nil)
