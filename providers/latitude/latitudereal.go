package main

import (
	"context"
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

// hostnamePrefix marks a server as BigFleet-managed and carries the machine id.
// Latitude's API exposes no settable+readable tag/label at create time — only
// the hostname is set at create AND returned on Get/List — so the hostname is
// the machine-id carrier (base32, lowercased: only [a-z2-7], DNS-safe). This
// mirrors Hetzner's encoded-label approach. Machine ids whose encoding would
// exceed the 63-char hostname limit fall back to the FileStore for restart
// recovery (the documented primary path) and a TOFU host-key pin.
const hostnamePrefix = "bigfleet-"

// latitudeRealConfig is the launch configuration for the production Latitude.sh
// client.
type latitudeRealConfig struct {
	Token   string
	Project string // project id or slug; every server op is scoped to it

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

// serverRecord is the per-server substrate state this provider legitimately owns
// (the kit owns everything else, in the FileStore): the pinned SSH host-key
// fingerprint and the per-server UserData resource id, so Configure/Drain can
// verify the host and DeleteInstance can tear down without leaking. It is
// in-memory; after a restart a server with no record TOFU-pins its host key on
// the next SSH connection (documented in security.md).
type serverRecord struct {
	hostKeyFP  string
	userDataID string
	clusterID  string
}

// latitudeReal is the production latitudeClient, backed by latitudesh-go-sdk.
// Inventory is recovered from server hostnames; the cluster-specific bootstrap
// and the drain are delivered over SSH on the pinned host key (Latitude exposes
// no in-guest command API).
type latitudeReal struct {
	cfg     latitudeRealConfig
	sdk     *latitudeshgosdk.Latitudesh
	ssh     *sshDelivery
	logger  *slog.Logger
	project string

	mu       sync.Mutex
	records  map[string]*serverRecord // serverID -> owned substrate state
	sshKeyID string                   // cached id of the registered SSH key
}

func newLatitudeReal(cfg latitudeRealConfig, logger *slog.Logger) (*latitudeReal, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("latitude: --token (or LATITUDESH_API_TOKEN) is required for the latitude backend")
	}
	if cfg.Project == "" {
		return nil, fmt.Errorf("latitude: --project (or LATITUDESH_PROJECT) is required for the latitude backend")
	}
	cfg.withDefaults()
	r := &latitudeReal{
		cfg:     cfg,
		sdk:     latitudeshgosdk.New(latitudeshgosdk.WithSecurity(cfg.Token)),
		logger:  logger,
		project: cfg.Project,
		records: make(map[string]*serverRecord),
	}
	if cfg.SSHSigner != nil {
		r.ssh = &sshDelivery{
			signer: cfg.SSHSigner,
			user:   cfg.SSHUser,
			onTOFU: func(serverID, fp string) {
				r.mu.Lock()
				rec := r.recordLocked(serverID)
				rec.hostKeyFP = fp
				r.mu.Unlock()
				if logger != nil {
					logger.Warn("pinned SSH host key on first use (no in-process pin)", "server", serverID)
				}
			},
		}
	}
	return r, nil
}

func (r *latitudeReal) recordLocked(serverID string) *serverRecord {
	rec, ok := r.records[serverID]
	if !ok {
		rec = &serverRecord{}
		r.records[serverID] = rec
	}
	return rec
}

// CreateServer deploys one bare-metal server and waits for it to power on. It is
// substrate-idempotent: a server already carrying this machine id's hostname is
// adopted rather than deployed again.
func (r *latitudeReal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	hostname := deployHostname(spec.MachineID)

	// Idempotency pre-check: a retried Create whose hostname already exists adopts
	// that server instead of deploying a duplicate.
	if existing, ok, err := r.serverByHostname(ctx, hostname); err == nil && ok {
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
	userDataID, err := r.createUserData(ctx, hostname, cloudInit)
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
		// hostname — adopt it instead of failing.
		if existing, ok, lerr := r.serverByHostname(ctx, hostname); lerr == nil && ok {
			return r.waitPoweredOn(ctx, existing.ServerID)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.Plan, err)
	}
	id := serverID(resp.GetServer())
	if id == "" {
		return serverInstance{}, fmt.Errorf("create server %s: empty server id", spec.Plan)
	}
	r.mu.Lock()
	rec := r.recordLocked(id)
	rec.hostKeyFP = hostKey.fingerprint
	rec.userDataID = userDataID
	r.mu.Unlock()
	return r.waitPoweredOn(ctx, id)
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
// created, idempotently (an already-gone server / resource is success).
func (r *latitudeReal) DeleteServer(ctx context.Context, serverIDStr string) error {
	_, err := r.sdk.Servers.Delete(ctx, serverIDStr, latitudeshgosdk.String("bigfleet-reclaim"))
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete server %s: %w", serverIDStr, err)
	}
	// Tear down the per-server UserData resource so it does not leak.
	r.mu.Lock()
	rec := r.records[serverIDStr]
	delete(r.records, serverIDStr)
	r.mu.Unlock()
	if rec != nil && rec.userDataID != "" {
		if _, derr := r.sdk.UserData.Delete(ctx, rec.userDataID); derr != nil && !isNotFound(derr) {
			if r.logger != nil {
				r.logger.Warn("failed to delete per-server user-data (will retry on next delete)", "user_data", rec.userDataID, "err", derr)
			}
		}
	}
	return nil
}

// DescribeManaged lists the project's servers and returns the BigFleet-managed
// ones (hostname carries the bigfleet- prefix + machine id).
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
	r.mu.Lock()
	r.recordLocked(srv.ServerID).clusterID = clusterID
	r.mu.Unlock()
	return nil
}

// DrainNode cordons + drains the kubelet over SSH and clears the binding.
func (r *latitudeReal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	if r.ssh == nil {
		// No SSH path: at least clear the in-memory binding.
		r.mu.Lock()
		r.recordLocked(srv.ServerID).clusterID = ""
		r.mu.Unlock()
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
	r.mu.Lock()
	r.recordLocked(srv.ServerID).clusterID = ""
	r.mu.Unlock()
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

// withPin attaches the in-process pinned host-key fingerprint to a server view.
func (r *latitudeReal) withPin(srv serverInstance) serverInstance {
	r.mu.Lock()
	if rec, ok := r.records[srv.ServerID]; ok && rec.hostKeyFP != "" {
		srv.HostKeyFP = rec.hostKeyFP
	}
	r.mu.Unlock()
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
		if a.Hostname != nil {
			out.MachineID = decodeHostname(*a.Hostname)
		}
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
	out = r.withPin(out)
	r.mu.Lock()
	if rec, ok := r.records[out.ServerID]; ok {
		out.ClusterID = rec.clusterID
	}
	r.mu.Unlock()
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

// createUserData stores a UserData resource holding the host-key cloud-init and
// returns its id, for reference in the server deploy.
func (r *latitudeReal) createUserData(ctx context.Context, hostname, content string) (string, error) {
	resp, err := r.sdk.UserData.CreateNew(ctx, operations.PostUserDataUserDataRequestBody{
		Data: operations.PostUserDataUserDataData{
			Type: operations.PostUserDataUserDataTypeUserData,
			Attributes: &operations.PostUserDataUserDataAttributes{
				Description: "bigfleet host-key bootstrap for " + hostname,
				Project:     latitudeshgosdk.String(r.project),
				Content:     base64.StdEncoding.EncodeToString([]byte(content)),
			},
		},
	})
	if err != nil {
		return "", err
	}
	if resp.UserDataObject == nil || resp.UserDataObject.Data == nil || resp.UserDataObject.Data.ID == nil {
		return "", fmt.Errorf("user-data create returned no id")
	}
	return *resp.UserDataObject.Data.ID, nil
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

// deployHostname derives a stable, DNS-safe, BigFleet-prefixed hostname carrying
// the machine id. Stable across a retried Create, so the substrate-idempotency
// pre-check can adopt the existing server.
func deployHostname(machineID string) string {
	name := hostnamePrefix + encodeMachineID(machineID)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// decodeHostname recovers the machine id from a BigFleet-managed hostname, or ""
// if the hostname is not ours or was truncated (recovery falls back to the
// FileStore).
func decodeHostname(hostname string) string {
	if !strings.HasPrefix(hostname, hostnamePrefix) {
		return ""
	}
	return decodeMachineID(strings.TrimPrefix(hostname, hostnamePrefix))
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
