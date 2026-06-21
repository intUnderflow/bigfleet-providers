package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	latitudeshgosdk "github.com/latitudesh/latitudesh-go-sdk"
	"github.com/latitudesh/latitudesh-go-sdk/models/components"
	"github.com/latitudesh/latitudesh-go-sdk/models/operations"

	"golang.org/x/crypto/ssh"
)

// hostnamePrefix marks a server as BigFleet-managed. The hostname is a
// collision-free HASH of the machine id (see deployHostname), not a reversible
// encoding — Latitude exposes no settable+readable tag/label on the server
// object, and a ≤63-char hostname cannot losslessly carry an arbitrary machine
// id. Identity is therefore keyed on the machine id via the provider-owned
// substrateIndex, never decoded from the hostname; the hostname is only a
// deterministic, human-readable deploy name and a collision-free idempotency
// backstop.
const hostnamePrefix = "bigfleet-"

// latitudeRealConfig is the launch configuration for the production Latitude.sh
// client.
type latitudeRealConfig struct {
	Token   string
	Project string // project id or slug; every server op is scoped to it

	// StatePath persists the substrateIndex (machine_id -> {serverID, host-key
	// fingerprint, UserData id, cluster binding}). Empty = in-memory only.
	StatePath string

	// SSHSigner authenticates the SSH session used by ApplyBootstrap / DrainNode
	// (Latitude has no in-guest command API). Nil disables SSH delivery.
	SSHSigner ssh.Signer
	SSHUser   string
	// BootstrapHookPath is the executable on the deployed OS that consumes the
	// delivered bootstrap blob (written to <path>.blob) and joins the cluster.
	BootstrapHookPath string

	// CreateWaitTimeout caps how long CreateServer waits for the server to power
	// on (the kit's Create timeout, carried on ctx, usually fires first).
	CreateWaitTimeout time.Duration
	// PollInterval is how often CreateServer / power-on polls the server status.
	PollInterval time.Duration
}

func (c *latitudeRealConfig) withDefaults() {
	if c.SSHUser == "" {
		c.SSHUser = "root"
	}
	if c.BootstrapHookPath == "" {
		c.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 25 * time.Minute // bare-metal deploys take minutes
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 10 * time.Second
	}
}

// latitudeReal is the production latitudeClient, backed by latitudesh-go-sdk.
// Identity (machine_id <-> serverID) and the per-machine substrate state (pinned
// host-key fingerprint, per-server UserData id, cluster binding) live in the
// provider-owned, persisted substrateIndex; the cluster-specific bootstrap and
// the drain are delivered over SSH on the pinned host key (Latitude exposes no
// in-guest command API).
type latitudeReal struct {
	cfg     latitudeRealConfig
	sdk     *latitudeshgosdk.Latitudesh
	ud      userDataAPI
	ssh     *sshDelivery
	logger  *slog.Logger
	project string
	index   *substrateIndex

	mu       sync.Mutex
	sshKeyID string // cached id of the registered SSH key
}

func newLatitudeReal(cfg latitudeRealConfig, logger *slog.Logger) (*latitudeReal, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("latitude: --token (or LATITUDESH_API_TOKEN) is required for the latitude backend")
	}
	if cfg.Project == "" {
		return nil, fmt.Errorf("latitude: --project (or LATITUDESH_PROJECT) is required for the latitude backend")
	}
	cfg.withDefaults()
	index, err := newSubstrateIndex(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	sdk := latitudeshgosdk.New(latitudeshgosdk.WithSecurity(cfg.Token))
	r := &latitudeReal{
		cfg:     cfg,
		sdk:     sdk,
		ud:      &sdkUserData{sdk: sdk},
		logger:  logger,
		project: cfg.Project,
		index:   index,
	}
	if cfg.SSHSigner != nil {
		r.ssh = &sshDelivery{
			signer: cfg.SSHSigner,
			user:   cfg.SSHUser,
			onTOFU: func(serverID, fp string) {
				// Persist the observed host key only for a server we own (in the
				// index); an orphan has no machine id to key it on (TOFU each time,
				// documented in security.md).
				if st, ok := r.index.machineByServer(serverID); ok {
					st.HostKeyFP = fp
					if err := r.index.upsert(st); err != nil && logger != nil {
						logger.Warn("failed to persist TOFU host-key pin", "server", serverID, "err", err)
					}
				}
				if logger != nil {
					logger.Warn("pinned SSH host key on first use (no prior pin)", "server", serverID)
				}
			},
		}
	}
	return r, nil
}

// CreateServer deploys one bare-metal server and waits for it to power on. It is
// substrate-idempotent: if a server already backs this machine id — found via the
// persisted index, or via the collision-free deploy hostname as a backstop — it
// is adopted rather than deployed again.
func (r *latitudeReal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	hostname := deployHostname(spec.MachineID)

	// Idempotency pre-check 1: the index already maps this machine id to a server.
	if st, ok := r.index.machineByID(spec.MachineID); ok && st.ServerID != "" {
		if srv, err := r.GetServer(ctx, st.ServerID); err == nil && srv.ServerID != "" {
			return r.waitPoweredOn(ctx, srv.ServerID)
		}
	}
	// Idempotency pre-check 2 (backstop after an index loss / a crash between
	// Servers.Create and the index write): a server already carrying this machine
	// id's collision-free deploy hostname. An exact hostname match means the same
	// machine id (the hostname is a sha256 hash), so this never adopts the wrong
	// server.
	if existing, ok, err := r.serverByHostname(ctx, hostname); err == nil && ok {
		r.recordServer(spec.MachineID, existing.ServerID, "", "")
		return r.waitPoweredOn(ctx, existing.ServerID)
	}

	sshKeyID, err := r.ensureSSHKey(ctx)
	if err != nil {
		return serverInstance{}, fmt.Errorf("ensure ssh key: %w", err)
	}

	// Mint an SSH host key and inject it via a per-server UserData resource, so the
	// host boots presenting a key we already know. Its fingerprint is pinned and
	// verified on every later Configure/Drain SSH connection. The cluster-JOIN
	// SECRET is NOT here — it is delivered later over SSH by ApplyBootstrap.
	hostKey, err := generateHostKey()
	if err != nil {
		return serverInstance{}, err
	}
	cloudInit, err := buildUserData(spec.BaseUserData, hostKey.cloudConfig())
	if err != nil {
		return serverInstance{}, fmt.Errorf("assemble user-data: %w", err)
	}
	userDataID, err := r.createUserData(ctx, spec.MachineID, cloudInit)
	if err != nil {
		return serverInstance{}, fmt.Errorf("create user-data: %w", err)
	}

	plan := operations.CreateServerPlan(spec.Plan)
	site := operations.CreateServerSite(spec.Site)
	os := operations.CreateServerOperatingSystem(spec.OperatingSystem)
	billing := operations.CreateServerBillingHourly
	attrs := &operations.CreateServerServersAttributes{
		Project:         latitudeshgosdk.String(r.project),
		Plan:            &plan,
		Site:            &site,
		OperatingSystem: &os,
		Hostname:        latitudeshgosdk.String(hostname),
		SSHKeys:         []string{sshKeyID},
		UserData:        latitudeshgosdk.String(userDataID),
		Billing:         &billing,
	}
	req := operations.CreateServerServersRequestBody{
		Data: &operations.CreateServerServersData{
			Type:       operations.CreateServerServersTypeServers,
			Attributes: attrs,
		},
	}
	resp, err := r.sdk.Servers.Create(ctx, req)
	if err != nil {
		// A retried Create that raced an earlier success may now find the server by
		// hostname — adopt it instead of failing. The UserData we just created is not
		// attached to a server we own (the adopted server carries its own), so tear
		// it down rather than leak it.
		r.deleteUserData(ctx, userDataID)
		if existing, ok, lerr := r.serverByHostname(ctx, hostname); lerr == nil && ok {
			r.recordServer(spec.MachineID, existing.ServerID, "", "")
			return r.waitPoweredOn(ctx, existing.ServerID)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.Plan, err)
	}
	id := serverID(resp.GetServer())
	if id == "" {
		// The deploy did not return an id we can own, so the just-created UserData
		// would leak — tear it down.
		r.deleteUserData(ctx, userDataID)
		return serverInstance{}, fmt.Errorf("create server %s: empty server id", spec.Plan)
	}
	r.recordServer(spec.MachineID, id, hostKey.fingerprint, userDataID)
	return r.waitPoweredOn(ctx, id)
}

// recordServer upserts the machine's substrate state into the persisted index,
// preserving any existing host-key/UserData when the new values are empty.
func (r *latitudeReal) recordServer(machineID, serverIDStr, hostKeyFP, userDataID string) {
	st := machineState{MachineID: machineID, ServerID: serverIDStr, HostKeyFP: hostKeyFP, UserDataID: userDataID}
	if prev, ok := r.index.machineByID(machineID); ok {
		if st.HostKeyFP == "" {
			st.HostKeyFP = prev.HostKeyFP
		}
		if st.UserDataID == "" {
			st.UserDataID = prev.UserDataID
		}
		st.ClusterID = prev.ClusterID
	}
	if err := r.index.upsert(st); err != nil && r.logger != nil {
		r.logger.Warn("failed to persist substrate index", "machine", machineID, "server", serverIDStr, "err", err)
	}
}

// waitPoweredOn polls until the server reports status `on`, so the kit's IDLE
// means "reachable host" and the following Configure does not race a deploying
// server. ctx (the kit's Create timeout) cancels it.
func (r *latitudeReal) waitPoweredOn(ctx context.Context, id string) (serverInstance, error) {
	deadline := time.Now().Add(r.cfg.CreateWaitTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		srv, err := r.GetServer(ctx, id)
		if err == nil && srv.PoweredOn {
			return srv, nil
		}
		select {
		case <-ctx.Done():
			return serverInstance{}, fmt.Errorf("waiting for server %s to power on: %w", id, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return serverInstance{}, fmt.Errorf("server %s did not power on within %s", id, r.cfg.CreateWaitTimeout)
			}
		}
	}
}

// DeleteServer deprovisions the server AND the per-server UserData resource it
// created, idempotently (an already-gone server / resource is success). The
// machine id is passed by the caller (from req.Machine.ID) so the UserData id can
// be recovered by its machine-id-keyed description even when the persisted index
// is unavailable — never derived from the lossy hostname.
func (r *latitudeReal) DeleteServer(ctx context.Context, serverIDStr, machineID string) error {
	// Resolve the per-server UserData id to tear down: the persisted index is the
	// fast path; if it lacks the entry (e.g. a lost state file) recover it by its
	// machine-id-keyed description so the orphan resource — which holds a cleartext
	// host private key — is not leaked.
	userDataID := ""
	if st, ok := r.index.machineByServer(serverIDStr); ok {
		userDataID = st.UserDataID
		if machineID == "" {
			machineID = st.MachineID
		}
	}
	if userDataID == "" && machineID != "" {
		userDataID = r.findUserDataID(ctx, userDataDescription(machineID))
	}

	_, err := r.sdk.Servers.Delete(ctx, serverIDStr, latitudeshgosdk.String("bigfleet-reclaim"))
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete server %s: %w", serverIDStr, err)
	}
	if rerr := r.index.removeByServer(serverIDStr); rerr != nil && r.logger != nil {
		r.logger.Warn("failed to persist substrate index on delete", "server", serverIDStr, "err", rerr)
	}
	r.deleteUserData(ctx, userDataID)
	return nil
}

// DescribeManaged lists the project's servers and returns the BigFleet-managed
// ones (hostname carries the bigfleet- prefix), mapping each back to its machine
// id via the persisted index.
func (r *latitudeReal) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	var out []serverInstance
	page := int64(1)
	for {
		resp, err := r.sdk.Servers.List(ctx, operations.GetServersRequest{
			FilterProject: latitudeshgosdk.String(r.project),
			PageSize:      latitudeshgosdk.Int64(50),
			PageNumber:    latitudeshgosdk.Int64(page),
		})
		if err != nil {
			return nil, fmt.Errorf("list servers: %w", err)
		}
		if resp.Servers == nil || len(resp.Servers.GetData()) == 0 {
			break
		}
		for i := range resp.Servers.Data {
			srv := r.dataToServerInstance(&resp.Servers.Data[i])
			if srv.MachineID == "" && !strings.HasPrefix(hostnameOf(&resp.Servers.Data[i]), hostnamePrefix) {
				continue // not ours
			}
			out = append(out, srv)
		}
		if len(resp.Servers.Data) < 50 {
			break
		}
		page++
	}
	return out, nil
}

// GetServer returns the current substrate view of one server by id.
func (r *latitudeReal) GetServer(ctx context.Context, serverIDStr string) (serverInstance, error) {
	resp, err := r.sdk.Servers.Get(ctx, serverIDStr, nil)
	if err != nil {
		return serverInstance{}, fmt.Errorf("get server %s: %w", serverIDStr, err)
	}
	if resp.GetServer() == nil || resp.GetServer().GetData() == nil {
		return serverInstance{}, fmt.Errorf("get server %s: empty response", serverIDStr)
	}
	return r.dataToServerInstance(resp.GetServer().GetData()), nil
}

// PowerOn powers a server on (idempotent: a server already on is success).
func (r *latitudeReal) PowerOn(ctx context.Context, serverIDStr string) error {
	req := operations.CreateServerActionServersRequestBody{
		Data: operations.CreateServerActionServersData{
			Type: operations.CreateServerActionServersTypeActions,
			Attributes: &operations.CreateServerActionServersAttributes{
				Action: operations.CreateServerActionActionPowerOn,
			},
		},
	}
	if _, err := r.sdk.Servers.RunAction(ctx, serverIDStr, req); err != nil {
		return fmt.Errorf("power on server %s: %w", serverIDStr, err)
	}
	return nil
}

// ApplyBootstrap delivers the opaque bootstrap blob over SSH on the pinned host
// key and records the cluster binding. The caller (backend) has already
// EnsureRunning'd the server.
func (r *latitudeReal) ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
	if r.ssh == nil {
		return fmt.Errorf("configure: SSH delivery disabled (set --ssh-key); cannot deliver bootstrap to %s", srv.ServerID)
	}
	srv = r.withPin(srv)
	if srv.PublicIPv4 == "" {
		fresh, err := r.GetServer(ctx, srv.ServerID)
		if err != nil {
			return fmt.Errorf("configure: %w", err)
		}
		srv.PublicIPv4 = fresh.PublicIPv4
	}
	blob := base64.StdEncoding.EncodeToString(bootstrap) // base64 -d is universally available
	hook := shellQuote(r.cfg.BootstrapHookPath)
	blobPath := shellQuote(r.cfg.BootstrapHookPath + ".blob")
	script := fmt.Sprintf(
		"set -euo pipefail; umask 077; mkdir -p \"$(dirname %s)\"; echo %s | base64 -d > %s; %s %s",
		blobPath, shellQuote(blob), blobPath, hook, shellQuote(clusterID))
	if err := r.ssh.run(ctx, srv, script); err != nil {
		return err
	}
	if err := r.index.setCluster(srv.ServerID, clusterID); err != nil && r.logger != nil {
		r.logger.Warn("failed to persist cluster binding", "server", srv.ServerID, "err", err)
	}
	return nil
}

// DrainNode cordons + drains the kubelet over SSH and clears the binding.
func (r *latitudeReal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	if r.ssh == nil {
		// No SSH path: at least clear the binding.
		_ = r.index.setCluster(srv.ServerID, "")
		return nil
	}
	srv = r.withPin(srv)
	if srv.PublicIPv4 == "" {
		fresh, err := r.GetServer(ctx, srv.ServerID)
		if err != nil {
			return fmt.Errorf("drain: %w", err)
		}
		srv.PublicIPv4 = fresh.PublicIPv4
	}
	grace := gracePeriodSeconds
	if grace <= 0 {
		grace = 1
	}
	script := fmt.Sprintf(
		"set -euo pipefail; node=$(hostname -f 2>/dev/null || hostname); "+
			"kubectl cordon \"$node\" || true; "+
			"kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds",
		grace, grace)
	if err := r.ssh.run(ctx, srv, script); err != nil {
		return err
	}
	if err := r.index.setCluster(srv.ServerID, ""); err != nil && r.logger != nil {
		r.logger.Warn("failed to persist cluster unbind", "server", srv.ServerID, "err", err)
	}
	return nil
}

// PriceUSD returns the hourly USD price for a plan in the given site.
func (r *latitudeReal) PriceUSD(ctx context.Context, plan, site string) (float64, error) {
	resp, err := r.sdk.Plans.List(ctx, operations.GetPlansRequest{
		FilterSlug: latitudeshgosdk.String(plan),
	})
	if err != nil {
		return 0, err
	}
	if resp.Object == nil {
		return 0, fmt.Errorf("no plan data for %s", plan)
	}
	for i := range resp.Object.Data {
		pd := &resp.Object.Data[i]
		if pd.Attributes == nil || pd.Attributes.Slug == nil || *pd.Attributes.Slug != plan {
			continue
		}
		for j := range pd.Attributes.Regions {
			reg := &pd.Attributes.Regions[j]
			if !regionMatchesSite(reg, site) {
				continue
			}
			if reg.Pricing != nil && reg.Pricing.Usd != nil && reg.Pricing.Usd.Hour != nil {
				return *reg.Pricing.Usd.Hour, nil
			}
		}
	}
	return 0, fmt.Errorf("no USD hourly price for %s in %s", plan, site)
}

// DescribePlanCapacities resolves vCPU + memory for the given plan slugs.
func (r *latitudeReal) DescribePlanCapacities(ctx context.Context, plans []string) (map[string]planCapacity, error) {
	out := make(map[string]planCapacity, len(plans))
	for _, slug := range plans {
		resp, err := r.sdk.Plans.List(ctx, operations.GetPlansRequest{
			FilterSlug: latitudeshgosdk.String(slug),
		})
		if err != nil {
			return nil, err
		}
		if resp.Object == nil {
			continue
		}
		for i := range resp.Object.Data {
			pd := &resp.Object.Data[i]
			if pd.Attributes == nil || pd.Attributes.Slug == nil || *pd.Attributes.Slug != slug {
				continue
			}
			if c, ok := planCapacityOf(pd.Attributes.Specs); ok {
				out[slug] = c
			}
		}
	}
	return out, nil
}

// --- helpers --------------------------------------------------------------

// withPin attaches the pinned host-key fingerprint (from the index) to a server
// view, so SSH verifies the host against the key pinned at deploy.
func (r *latitudeReal) withPin(srv serverInstance) serverInstance {
	if st, ok := r.index.machineByServer(srv.ServerID); ok && st.HostKeyFP != "" {
		srv.HostKeyFP = st.HostKeyFP
	}
	return srv
}

func (r *latitudeReal) dataToServerInstance(d *components.ServerData) serverInstance {
	out := serverInstance{Running: true}
	if d == nil {
		return out
	}
	if d.ID != nil {
		out.ServerID = *d.ID
	}
	a := d.Attributes
	if a != nil {
		if a.Site != nil {
			out.Site = *a.Site
		}
		if a.PrimaryIpv4 != nil {
			out.PublicIPv4 = *a.PrimaryIpv4
		}
		if a.Plan != nil && a.Plan.Slug != nil {
			out.Plan = *a.Plan.Slug
		}
		if a.Status != nil {
			st := *a.Status
			out.PoweredOn = st == components.ServerDataStatusOn
			out.Running = st != components.ServerDataStatusFailedDeployment
		}
		if out.Site == "" && a.Region != nil && a.Region.Site != nil && a.Region.Site.Slug != nil {
			out.Site = *a.Region.Site.Slug
		}
	}
	// Identity + owned substrate state come from the persisted index, keyed on the
	// server id — never decoded from the (lossy) hostname.
	if st, ok := r.index.machineByServer(out.ServerID); ok {
		out.MachineID = st.MachineID
		out.ClusterID = st.ClusterID
		if st.HostKeyFP != "" {
			out.HostKeyFP = st.HostKeyFP
		}
	}
	return out
}

func (r *latitudeReal) serverByHostname(ctx context.Context, hostname string) (serverInstance, bool, error) {
	resp, err := r.sdk.Servers.List(ctx, operations.GetServersRequest{
		FilterProject:  latitudeshgosdk.String(r.project),
		FilterHostname: latitudeshgosdk.String(hostname),
		PageSize:       latitudeshgosdk.Int64(10),
	})
	if err != nil {
		return serverInstance{}, false, err
	}
	if resp.Servers == nil {
		return serverInstance{}, false, nil
	}
	for i := range resp.Servers.Data {
		if hostnameOf(&resp.Servers.Data[i]) == hostname {
			return r.dataToServerInstance(&resp.Servers.Data[i]), true, nil
		}
	}
	return serverInstance{}, false, nil
}

// ensureSSHKey registers (once) the public key matching the configured signer so
// deployed servers authorise our SSH login, caching the key id.
func (r *latitudeReal) ensureSSHKey(ctx context.Context) (string, error) {
	r.mu.Lock()
	if r.sshKeyID != "" {
		id := r.sshKeyID
		r.mu.Unlock()
		return id, nil
	}
	r.mu.Unlock()
	if r.cfg.SSHSigner == nil {
		return "", fmt.Errorf("no --ssh-key configured; cannot register an SSH key for Configure delivery")
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(r.cfg.SSHSigner.PublicKey())))
	name := "bigfleet-" + hostKeyFingerprint(r.cfg.SSHSigner.PublicKey())[:16]

	// Reuse an already-registered key with this name if present.
	if list, err := r.sdk.SSHKeys.ListAll(ctx, operations.GetSSHKeysRequest{
		FilterProject: latitudeshgosdk.String(r.project),
	}); err == nil && list.SSHKeys != nil {
		for i := range list.SSHKeys.Data {
			k := &list.SSHKeys.Data[i]
			if k.ID != nil && k.Attributes != nil && k.Attributes.Name != nil && *k.Attributes.Name == name {
				r.mu.Lock()
				r.sshKeyID = *k.ID
				r.mu.Unlock()
				return *k.ID, nil
			}
		}
	}

	resp, err := r.sdk.SSHKeys.Create(ctx, operations.PostSSHKeySSHKeysRequestBody{
		Data: operations.PostSSHKeySSHKeysData{
			Type: operations.PostSSHKeySSHKeysTypeSSHKeys,
			Attributes: &operations.PostSSHKeySSHKeysAttributes{
				Name:      latitudeshgosdk.String(name),
				Project:   latitudeshgosdk.String(r.project),
				PublicKey: latitudeshgosdk.String(pub),
			},
		},
	})
	if err != nil {
		return "", err
	}
	if resp.Object == nil || resp.Object.Data == nil || resp.Object.Data.ID == nil {
		return "", fmt.Errorf("ssh key create returned no id")
	}
	r.mu.Lock()
	r.sshKeyID = *resp.Object.Data.ID
	r.mu.Unlock()
	return *resp.Object.Data.ID, nil
}

// userDataDescription is the stable, machine-id-keyed description stamped on a
// server's UserData resource, so its id can be recovered (via UserData.List) when
// the persisted index is unavailable and the resource torn down rather than
// leaked. Keyed on the machine id directly (not the lossy hostname), so recovery
// from the real machine id is exact.
func userDataDescription(machineID string) string {
	return "bigfleet host-key for machine " + machineID
}

// createUserData stores a UserData resource holding the host-key cloud-init and
// returns its id, for reference in the server deploy.
func (r *latitudeReal) createUserData(ctx context.Context, machineID, content string) (string, error) {
	return r.ud.create(ctx, r.project, userDataDescription(machineID), base64.StdEncoding.EncodeToString([]byte(content)))
}

// findUserDataID recovers a UserData resource id by its machine-id-keyed
// description — a best-effort fallback for when the persisted index lacks the
// entry. Returns "" if none matches or the listing fails.
func (r *latitudeReal) findUserDataID(ctx context.Context, description string) string {
	items, err := r.ud.list(ctx, r.project)
	if err != nil {
		return ""
	}
	for _, it := range items {
		if it.Description == description {
			return it.ID
		}
	}
	return ""
}

// deleteUserData removes a UserData resource, idempotently.
func (r *latitudeReal) deleteUserData(ctx context.Context, userDataID string) {
	if userDataID == "" {
		return
	}
	if err := r.ud.delete(ctx, userDataID); err != nil && !isNotFound(err) {
		if r.logger != nil {
			r.logger.Warn("failed to delete per-server user-data", "user_data", userDataID, "err", err)
		}
	}
}

// serverID extracts the id from a created/fetched server.
func serverID(s *components.Server) string {
	if s == nil || s.Data == nil || s.Data.ID == nil {
		return ""
	}
	return *s.Data.ID
}

func hostnameOf(d *components.ServerData) string {
	if d == nil || d.Attributes == nil || d.Attributes.Hostname == nil {
		return ""
	}
	return *d.Attributes.Hostname
}

// deployHostname derives a stable, DNS-safe, BigFleet-prefixed deploy name from a
// COLLISION-FREE hash of the machine id: base32(sha256(machine_id)). It is not
// reversible (identity is keyed on the machine id via the index), but an exact
// hostname match is a reliable same-machine-id signal for the idempotency
// backstop, and distinct machine ids never collide (unlike a truncated encoding).
func deployHostname(machineID string) string {
	sum := sha256.Sum256([]byte(machineID))
	return hostnamePrefix + strings.ToLower(machineIDEncoding.EncodeToString(sum[:]))
}

func regionMatchesSite(reg *components.PlanDataRegions, site string) bool {
	if reg == nil || reg.Name == nil {
		return false
	}
	// Latitude reports a plan's per-region pricing keyed by the region/site name;
	// match it case-insensitively against the offering's site slug.
	return strings.EqualFold(*reg.Name, site)
}

func planCapacityOf(specs *components.Specs) (planCapacity, bool) {
	if specs == nil || specs.CPU == nil || specs.Memory == nil {
		return planCapacity{}, false
	}
	cores := 0.0
	if specs.CPU.Cores != nil {
		cores = *specs.CPU.Cores
	}
	count := 1.0
	if specs.CPU.Count != nil && *specs.CPU.Count > 0 {
		count = *specs.CPU.Count
	}
	vcpu := int(cores * count)
	if vcpu <= 0 {
		return planCapacity{}, false
	}
	memGB := 0.0
	if specs.Memory.Total != nil {
		memGB = *specs.Memory.Total
	}
	if memGB <= 0 {
		return planCapacity{}, false
	}
	return planCapacity{VCPU: vcpu, MemMiB: int64(memGB * 1024)}, true
}

// isNotFound reports whether err looks like a 404 from the Latitude API, so
// delete paths can be idempotent.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "404") || strings.Contains(s, "not found") || strings.Contains(s, "not_found")
}

var _ latitudeClient = (*latitudeReal)(nil)
