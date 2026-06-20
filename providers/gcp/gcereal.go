package main

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// BigFleet recovery keys. bigfleet-managed is a LABEL that marks our instances
// so DescribeManaged (an AggregatedList label filter) never touches anything
// else; bigfleet-capacity is a short, label-safe LABEL. The machine id and
// cluster id, by contrast, are arbitrary-length, mixed-case strings (a slotID
// like "gcp-us-central1/Spot/n2-standard-8/us-central1-a/000" is ~56 bytes) that
// would overflow GCE's 63-char label-VALUE limit, so they are stored as instance
// METADATA — which has no length limit and accepts arbitrary bytes — and
// round-trip verbatim.
const (
	labelManaged  = "bigfleet-managed"  // label: marks our instances for the AggregatedList filter
	labelCapacity = "bigfleet-capacity" // label: canonical capacity string (short, label-safe)

	metaMachineID = "bigfleet-machine-id" // metadata: the BigFleet machine id (verbatim)
	metaCluster   = "bigfleet-cluster"    // metadata: the bound cluster id (verbatim), absent when unbound
)

// GCE-native metadata keys the provider sets at Insert.
const (
	// startupScriptKey carries the generic, cluster-agnostic pre-binding boot
	// script (NOT the cluster bootstrap — that is delivered in-band over SSH).
	startupScriptKey = "startup-script"
	// sshKeysKey authorises the provider's SSH client key for the SSH user (the
	// guest agent provisions it into the user's authorized_keys).
	sshKeysKey = "ssh-keys"
	// enableOSLoginKey is set false so metadata-based ssh-keys authorisation is
	// honoured (OS Login would otherwise ignore ssh-keys metadata).
	enableOSLoginKey = "enable-oslogin"
	// userDataKey carries the cloud-init payload (the injected SSH host key) on
	// cloud-init-enabled images.
	userDataKey = "user-data"
)

// nameEncoding renders an opaque token into a DNS-safe instance-name suffix
// (lowercase base32 without padding → only [a-z2-7]). Used by instanceName so a
// retried Insert maps to the same instance.
var nameEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// gceRealConfig is the launch configuration for the production GCE client.
type gceRealConfig struct {
	Project    string
	Region     string
	Image      string // boot disk source image (family or full URL)
	Network    string // VPC network for the NIC
	Subnetwork string // optional subnetwork for the NIC
	DiskSizeGB int64
	// InstanceServiceAccount is the service account the launched instances run
	// as (distinct from the provider's own identity). Empty → the project
	// default compute service account.
	InstanceServiceAccount string

	// SSHSigner authenticates the in-band SSH session used by ApplyBootstrap /
	// DrainNode to reach the running host. Its public key is authorised on the
	// instance via ssh-keys metadata at Insert. Nil disables SSH delivery (a
	// dev/fake-only mode; Configure then fails).
	SSHSigner ssh.Signer
	// SSHUser is the login the provider connects as (the guest agent creates it
	// from ssh-keys metadata).
	SSHUser string
	// BootstrapHookPath is the executable on the base image that consumes the
	// delivered bootstrap blob (written to <path>.blob) and joins the cluster.
	BootstrapHookPath string
	// UseExternalIP reaches the instance over its external IP (an ephemeral NAT
	// access config is added at Insert). Default false: reach it over its internal
	// IP (the provider runs in the same VPC).
	UseExternalIP bool

	// CreateWaitTimeout caps how long Insert waits for the operation + RUNNING
	// (the kit's Create timeout, carried on ctx, usually fires first).
	CreateWaitTimeout time.Duration
	// PollInterval is how often Insert polls the instance status.
	PollInterval time.Duration
}

func (c *gceRealConfig) withDefaults() {
	if c.Image == "" {
		c.Image = "projects/debian-cloud/global/images/family/debian-12"
	}
	if c.Network == "" {
		c.Network = "global/networks/default"
	}
	if c.DiskSizeGB <= 0 {
		c.DiskSizeGB = 20
	}
	if c.SSHUser == "" {
		c.SSHUser = "bigfleet"
	}
	if c.BootstrapHookPath == "" {
		c.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 5 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
}

// gceReal is the production gceClient, backed by cloud.google.com/go/compute.
// Inventory is recovered from instance labels + metadata; the cluster-specific
// bootstrap is delivered **in-band over SSH** to the already-running host (no
// reboot, secret never persisted), mirroring the certified AWS (SSM) and Hetzner
// (SSH) providers. The instance's SSH host key is injected at Insert and pinned,
// so Configure/Drain verify the host. One process per region; DescribeManaged
// uses AggregatedList and filters to the region's zones.
type gceReal struct {
	cfg          gceRealConfig
	instances    *compute.InstancesClient
	machineTypes *compute.MachineTypesClient
	logger       *slog.Logger
}

func newGCEReal(ctx context.Context, cfg gceRealConfig, logger *slog.Logger) (*gceReal, error) {
	if cfg.Project == "" {
		return nil, fmt.Errorf("gce: --project is required for the gcp backend")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("gce: --region is required for the gcp backend")
	}
	cfg.withDefaults()
	inst, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gce: instances client: %w", err)
	}
	mt, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		_ = inst.Close()
		return nil, fmt.Errorf("gce: machine-types client: %w", err)
	}
	return &gceReal{cfg: cfg, instances: inst, machineTypes: mt, logger: logger}, nil
}

// Close releases the GCE client connections.
func (r *gceReal) Close() error {
	err := r.instances.Close()
	if mtErr := r.machineTypes.Close(); mtErr != nil && err == nil {
		err = mtErr
	}
	return err
}

func (r *gceReal) Insert(ctx context.Context, spec instanceSpec) (gceInstance, error) {
	name := instanceName(spec)
	res := &computepb.Instance{
		Name:        proto.String(name),
		MachineType: proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", spec.Zone, spec.MachineType)),
		Labels: map[string]string{
			labelManaged:  "true",
			labelCapacity: spec.Capacity,
		},
		Disks: []*computepb.AttachedDisk{{
			Boot:       proto.Bool(true),
			AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{
				SourceImage: proto.String(r.cfg.Image),
				DiskSizeGb:  proto.Int64(r.cfg.DiskSizeGB),
			},
		}},
		NetworkInterfaces: []*computepb.NetworkInterface{r.networkInterface()},
	}
	// The machine id (and later the cluster id) live in metadata, not labels —
	// they exceed the 63-char label-value limit. The base startup script and the
	// SSH wiring for in-band Configure/Drain ride along.
	var items []*computepb.Items
	if spec.MachineID != "" {
		items = append(items, metadataItem(metaMachineID, spec.MachineID))
	}
	if len(spec.BaseStartupScript) > 0 {
		items = append(items, metadataItem(startupScriptKey, string(spec.BaseStartupScript)))
	}
	// Authorise the provider's SSH client key and inject a pinned host key so the
	// in-band Configure/Drain can connect and verify the host.
	if r.cfg.SSHSigner != nil {
		hk, err := generateHostKey()
		if err != nil {
			return gceInstance{}, err
		}
		userData, err := buildCloudInitUserData(nil, hk.cloudConfig())
		if err != nil {
			return gceInstance{}, fmt.Errorf("assemble user-data: %w", err)
		}
		clientKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(r.cfg.SSHSigner.PublicKey())))
		items = append(items,
			metadataItem(sshKeysKey, r.cfg.SSHUser+":"+clientKey),
			metadataItem(enableOSLoginKey, "false"),
			metadataItem(userDataKey, userData),
			metadataItem(metaHostKeyFP, hk.fingerprint),
		)
	}
	if len(items) > 0 {
		res.Metadata = &computepb.Metadata{Items: items}
	}
	if spec.Spot {
		res.Scheduling = &computepb.Scheduling{
			ProvisioningModel: proto.String("SPOT"),
			AutomaticRestart:  proto.Bool(false),
		}
	}
	if r.cfg.InstanceServiceAccount != "" {
		res.ServiceAccounts = []*computepb.ServiceAccount{{
			Email:  proto.String(r.cfg.InstanceServiceAccount),
			Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
		}}
	}

	op, err := r.instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          r.cfg.Project,
		Zone:             spec.Zone,
		InstanceResource: res,
	})
	if err != nil {
		// A retried Insert whose name already exists is the idempotent case:
		// recover the existing instance instead of failing. Route it through
		// waitRunning (not a bare Get) so recovery is symmetric with the success
		// path — a retry that lands before the instance is RUNNING must still wait,
		// preserving the "Idle == reachable host" invariant.
		if isAlreadyExists(err) {
			return r.waitRunning(ctx, spec.Zone, name)
		}
		return gceInstance{}, fmt.Errorf("insert instance %s: %w", name, err)
	}
	if err := op.Wait(ctx); err != nil {
		return gceInstance{}, fmt.Errorf("wait for insert %s: %w", name, err)
	}
	return r.waitRunning(ctx, spec.Zone, name)
}

// waitRunning polls until the instance reaches RUNNING (so the kit's IDLE means
// "reachable host" and the immediately-following Configure does not race a
// still-booting instance). ctx (the kit's Create timeout) cancels it.
func (r *gceReal) waitRunning(ctx context.Context, zone, name string) (gceInstance, error) {
	deadline := time.Now().Add(r.cfg.CreateWaitTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		inst, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{
			Project:  r.cfg.Project,
			Zone:     zone,
			Instance: name,
		})
		if err != nil {
			return gceInstance{}, fmt.Errorf("get instance %s: %w", name, err)
		}
		// Block until the instance is actually RUNNING — not merely a live state
		// like PROVISIONING/STAGING — so the immediately-following Configure does
		// not race a still-booting host.
		if inst.GetStatus() == "RUNNING" {
			return r.toGCEInstance(inst), nil
		}
		select {
		case <-ctx.Done():
			return gceInstance{}, fmt.Errorf("waiting for instance %s to run: %w", name, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return gceInstance{}, fmt.Errorf("instance %s did not reach RUNNING within %s", name, r.cfg.CreateWaitTimeout)
			}
		}
	}
}

func (r *gceReal) DeleteInstance(ctx context.Context, zone, name string) error {
	op, err := r.instances.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  r.cfg.Project,
		Zone:     zone,
		Instance: name,
	})
	if err != nil {
		if isNotFound(err) {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("delete instance %s: %w", name, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait for delete %s: %w", name, err)
	}
	return nil
}

func (r *gceReal) DescribeManaged(ctx context.Context) ([]gceInstance, error) {
	it := r.instances.AggregatedList(ctx, &computepb.AggregatedListInstancesRequest{
		Project: r.cfg.Project,
		Filter:  proto.String(fmt.Sprintf("labels.%s=true", labelManaged)),
	})
	var out []gceInstance
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("aggregated list: %w", err)
		}
		if pair.Value == nil {
			continue
		}
		for _, inst := range pair.Value.Instances {
			gi := r.toGCEInstance(inst)
			// One process per region: ignore instances outside this region's zones.
			if !strings.HasPrefix(gi.Zone, r.cfg.Region+"-") {
				continue
			}
			out = append(out, gi)
		}
	}
	return out, nil
}

// ApplyBootstrap delivers the bootstrap blob in-band over SSH to the running
// host (no reboot, secret never persisted in metadata), then records the cluster
// binding in metadata only after the on-host hook succeeds.
func (r *gceReal) ApplyBootstrap(ctx context.Context, inst gceInstance, clusterID string, bootstrap []byte) error {
	if r.cfg.SSHSigner == nil {
		return fmt.Errorf("configure: SSH delivery disabled (set --ssh-key); cannot deliver bootstrap to %s", inst.Name)
	}
	live, err := r.ensureReachable(ctx, inst)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	// Deliver the opaque blob to <hook>.blob (umask 077) and run the image's hook,
	// which joins the cluster. The blob is base64'd for safe transport and never
	// persisted (control-plane metadata is not used for the secret). We wait for
	// the hook to SUCCEED, so a failed bootstrap surfaces as FAILED.
	blob := base64.StdEncoding.EncodeToString(bootstrap)
	hook := shellQuote(r.cfg.BootstrapHookPath)
	blobPath := shellQuote(r.cfg.BootstrapHookPath + ".blob")
	script := fmt.Sprintf(
		"set -euo pipefail; umask 077; sudo mkdir -p \"$(dirname %s)\"; echo %s | base64 -d | sudo tee %s >/dev/null; sudo %s %s",
		blobPath, shellQuote(blob), blobPath, hook, shellQuote(clusterID))
	if err := r.runSSH(ctx, live, script); err != nil {
		return err
	}
	// Record the binding only AFTER the hook actually succeeded, so a failed
	// Configure never leaves an instance recorded as bound to a cluster it never
	// joined.
	return r.recordCluster(ctx, inst.Zone, inst.Name, clusterID)
}

// DrainNode cordons and drains the kubelet over SSH (honouring the grace period),
// then clears the cluster binding — leaving the instance running but unbound. No
// reboot.
func (r *gceReal) DrainNode(ctx context.Context, inst gceInstance, gracePeriodSeconds int64) error {
	if r.cfg.SSHSigner == nil {
		// No SSH path: at least clear the binding metadata so inventory reflects
		// the unbound state.
		return r.clearCluster(ctx, inst.Zone, inst.Name)
	}
	live, err := r.ensureReachable(ctx, inst)
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
			"sudo kubectl cordon \"$node\" || true; "+
			"sudo kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds",
		grace, grace)
	if err := r.runSSH(ctx, live, script); err != nil {
		return err
	}
	return r.clearCluster(ctx, inst.Zone, inst.Name)
}

// recordCluster sets the bigfleet-cluster binding metadata, preserving the rest.
func (r *gceReal) recordCluster(ctx context.Context, zone, name, clusterID string) error {
	live, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{Project: r.cfg.Project, Zone: zone, Instance: name})
	if err != nil {
		return fmt.Errorf("configure: get instance %s: %w", name, err)
	}
	md := setMetadataItem(live.GetMetadata(), metaCluster, clusterID)
	if err := r.setMetadata(ctx, zone, name, md); err != nil {
		return fmt.Errorf("configure: record cluster binding %s: %w", name, err)
	}
	return nil
}

// clearCluster removes the bigfleet-cluster binding metadata, preserving the rest.
func (r *gceReal) clearCluster(ctx context.Context, zone, name string) error {
	live, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{Project: r.cfg.Project, Zone: zone, Instance: name})
	if err != nil {
		return fmt.Errorf("drain: get instance %s: %w", name, err)
	}
	md := removeMetadataItem(live.GetMetadata(), metaCluster)
	if err := r.setMetadata(ctx, zone, name, md); err != nil {
		return fmt.Errorf("drain: clear cluster binding %s: %w", name, err)
	}
	return nil
}

func (r *gceReal) DescribeMachineTypeCapacities(ctx context.Context, refs []machineTypeRef) (map[string]machineCapacity, error) {
	out := make(map[string]machineCapacity, len(refs))
	for _, ref := range refs {
		mt, err := r.machineTypes.Get(ctx, &computepb.GetMachineTypeRequest{
			Project:     r.cfg.Project,
			Zone:        ref.Zone,
			MachineType: ref.MachineType,
		})
		if err != nil {
			if isNotFound(err) {
				continue // omitted; caller falls back to the pinned table
			}
			return nil, fmt.Errorf("get machine type %s: %w", ref.MachineType, err)
		}
		out[ref.MachineType] = machineCapacity{
			VCPU:   int(mt.GetGuestCpus()),
			MemMiB: int64(mt.GetMemoryMb()),
		}
	}
	return out, nil
}

// --- helpers --------------------------------------------------------------

func (r *gceReal) networkInterface() *computepb.NetworkInterface {
	ni := &computepb.NetworkInterface{Network: proto.String(r.cfg.Network)}
	if r.cfg.Subnetwork != "" {
		ni.Subnetwork = proto.String(r.cfg.Subnetwork)
	}
	if r.cfg.UseExternalIP {
		// Ephemeral external IP so the provider can reach the host over SSH when it
		// is not in the same VPC. Default off (internal IP).
		ni.AccessConfigs = []*computepb.AccessConfig{{
			Name: proto.String("External NAT"),
			Type: proto.String("ONE_TO_ONE_NAT"),
		}}
	}
	return ni
}

func (r *gceReal) toGCEInstance(inst *computepb.Instance) gceInstance {
	md := inst.GetMetadata()
	out := gceInstance{
		Name:        inst.GetName(),
		Zone:        lastPathSegment(inst.GetZone()),
		MachineType: lastPathSegment(inst.GetMachineType()),
		MachineID:   metadataValue(md, metaMachineID),
		ClusterID:   metadataValue(md, metaCluster),
		Capacity:    inst.GetLabels()[labelCapacity],
		IP:          r.instanceIP(inst),
		HostKeyFP:   metadataValue(md, metaHostKeyFP),
		SelfLink:    inst.GetSelfLink(),
		Running:     isRunningStatus(inst.GetStatus()),
	}
	if sched := inst.GetScheduling(); sched != nil {
		out.Spot = sched.GetProvisioningModel() == "SPOT" || sched.GetPreemptible()
	}
	// We only ever Delete instances, never stop them, so a SPOT VM observed in
	// TERMINATED status was stopped by GCE — i.e. preempted.
	out.Preempted = out.Spot && inst.GetStatus() == "TERMINATED"
	return out
}

// instanceIP returns the address Configure/Drain reach the host at: the external
// IP when --use-external-ip is set (and one is assigned), else the primary
// internal IP (the provider runs in the same VPC).
func (r *gceReal) instanceIP(inst *computepb.Instance) string {
	nics := inst.GetNetworkInterfaces()
	if len(nics) == 0 {
		return ""
	}
	if r.cfg.UseExternalIP {
		for _, ac := range nics[0].GetAccessConfigs() {
			if ip := ac.GetNatIP(); ip != "" {
				return ip
			}
		}
	}
	return nics[0].GetNetworkIP()
}

// ensureReachable returns inst with a populated IP + host-key fingerprint,
// re-fetching the instance when the cached view lacks them (e.g. the minimal
// fallback view the backend's resolveHost builds when a transient describe
// missed it). SSH-based Configure/Drain need the address.
func (r *gceReal) ensureReachable(ctx context.Context, inst gceInstance) (gceInstance, error) {
	if inst.IP != "" {
		return inst, nil
	}
	live, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{Project: r.cfg.Project, Zone: inst.Zone, Instance: inst.Name})
	if err != nil {
		return inst, fmt.Errorf("look up instance %s: %w", inst.Name, err)
	}
	full := r.toGCEInstance(live)
	if full.IP == "" {
		return inst, fmt.Errorf("instance %s has no reachable IP for SSH delivery", inst.Name)
	}
	return full, nil
}

// runSSH dials the instance, runs script over one session, and returns an error
// unless it exits 0. The host key is verified against the fingerprint pinned at
// Insert (inst.HostKeyFP); a mismatch aborts as a possible MITM. An instance with
// no pin (an orphan, or an image that did not honour the injected key) is
// trust-on-first-used and its observed key persisted, so all later connections
// are verified.
func (r *gceReal) runSSH(ctx context.Context, inst gceInstance, script string) error {
	host := inst.IP
	if host == "" {
		return fmt.Errorf("ssh: no reachable IP for instance %s", inst.Name)
	}
	var tofuFP string
	cfg := &ssh.ClientConfig{
		User:            r.cfg.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(r.cfg.SSHSigner)},
		HostKeyCallback: hostKeyCallback(inst.HostKeyFP, func(fp string) { tofuFP = fp }),
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
	if inst.HostKeyFP == "" && tofuFP != "" {
		if r.logger != nil {
			r.logger.Warn("pinning SSH host key on first use (no pre-injected key)", "instance", inst.Name)
		}
		if err := r.setHostKeyFP(ctx, inst.Zone, inst.Name, tofuFP); err != nil && r.logger != nil {
			r.logger.Warn("failed to persist TOFU host-key pin", "instance", inst.Name, "err", err)
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

// setHostKeyFP persists a trust-on-first-use host-key fingerprint into metadata.
func (r *gceReal) setHostKeyFP(ctx context.Context, zone, name, fp string) error {
	live, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{Project: r.cfg.Project, Zone: zone, Instance: name})
	if err != nil {
		return err
	}
	return r.setMetadata(ctx, zone, name, setMetadataItem(live.GetMetadata(), metaHostKeyFP, fp))
}

func (r *gceReal) setMetadata(ctx context.Context, zone, name string, md *computepb.Metadata) error {
	op, err := r.instances.SetMetadata(ctx, &computepb.SetMetadataInstanceRequest{
		Project:          r.cfg.Project,
		Zone:             zone,
		Instance:         name,
		MetadataResource: md,
	})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}

// metadataItem builds a single metadata key/value item.
func metadataItem(key, value string) *computepb.Items {
	return &computepb.Items{Key: proto.String(key), Value: proto.String(value)}
}

// metadataValue returns the value of a metadata item by key, or "" if absent.
func metadataValue(md *computepb.Metadata, key string) string {
	for _, it := range md.GetItems() {
		if it.GetKey() == key {
			return it.GetValue()
		}
	}
	return ""
}

// setMetadataItem returns a copy of md with key set to value (preserving the
// fingerprint, which GCE requires on update, and all other items).
func setMetadataItem(md *computepb.Metadata, key, value string) *computepb.Metadata {
	out := &computepb.Metadata{Fingerprint: proto.String(md.GetFingerprint())}
	replaced := false
	for _, it := range md.GetItems() {
		if it.GetKey() == key {
			out.Items = append(out.Items, &computepb.Items{Key: proto.String(key), Value: proto.String(value)})
			replaced = true
			continue
		}
		out.Items = append(out.Items, &computepb.Items{Key: proto.String(it.GetKey()), Value: proto.String(it.GetValue())})
	}
	if !replaced {
		out.Items = append(out.Items, &computepb.Items{Key: proto.String(key), Value: proto.String(value)})
	}
	return out
}

// removeMetadataItem returns a copy of md without key (preserving the
// fingerprint and all other items).
func removeMetadataItem(md *computepb.Metadata, key string) *computepb.Metadata {
	out := &computepb.Metadata{Fingerprint: proto.String(md.GetFingerprint())}
	for _, it := range md.GetItems() {
		if it.GetKey() == key {
			continue
		}
		out.Items = append(out.Items, &computepb.Items{Key: proto.String(it.GetKey()), Value: proto.String(it.GetValue())})
	}
	return out
}

// instanceName derives a stable, DNS-safe (RFC1035) GCE instance name from the
// operation id (stable across a retried Insert), so a transport retry recreates
// under the same name and Insert is idempotent. A fresh operation cycle (e.g. a
// re-Create after Delete) gets a fresh operation id and so a fresh name.
func instanceName(spec instanceSpec) string {
	token := spec.IdempotencyToken
	if token == "" {
		token = spec.MachineID
	}
	name := "bf-" + strings.ToLower(nameEncoding.EncodeToString([]byte(token)))
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func lastPathSegment(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// isRunningStatus reports whether a GCE instance status is a live state.
func isRunningStatus(status string) bool {
	switch status {
	case "PROVISIONING", "STAGING", "RUNNING", "REPAIRING":
		return true
	default: // STOPPING, SUSPENDING, SUSPENDED, TERMINATED, DEPROVISIONING
		return false
	}
}

func isAlreadyExists(err error) bool { return apiErrorCode(err) == 409 }
func isNotFound(err error) bool      { return apiErrorCode(err) == 404 }

// apiErrorCode extracts the HTTP status code from a googleapi error, or 0.
func apiErrorCode(err error) int {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code
	}
	return 0
}

var _ gceClient = (*gceReal)(nil)
