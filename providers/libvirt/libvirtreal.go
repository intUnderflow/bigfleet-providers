package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"
)

// machineIDEncoding encodes a (possibly slash-bearing) BigFleet machine id into
// a libvirt-domain-name-safe token (lowercase base32, no padding → only
// [a-z2-7]). The real domain name is "bigfleet-<token>"; the machine id is also
// stored verbatim in the domain's bigfleet metadata for recovery.
var machineIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// domainName derives a deterministic, libvirt-safe domain name from a token (the
// operation id, or the machine id when there is no op id). Short tokens use a
// readable lowercase-base32 encoding; a token whose encoding would overflow the
// length budget is named by a SHA-256 hash instead of being truncated, so two
// distinct long tokens can never alias to the same domain name.
func domainName(token string) string {
	const maxLen = 60 // libvirt domain names are generous, but keep it sane
	enc := strings.ToLower(machineIDEncoding.EncodeToString([]byte(token)))
	name := "bigfleet-" + enc
	if len(name) > maxLen {
		sum := sha256.Sum256([]byte(token))
		name = "bigfleet-" + hex.EncodeToString(sum[:20]) // 40 hex chars: 160-bit, collision-resistant
	}
	return name
}

// libvirtRealConfig is the launch configuration for the production go-libvirt
// client.
type libvirtRealConfig struct {
	// Connections maps each zone to the libvirt URI of the host that backs it.
	Connections []hostConn
	// Image is the golden base-image volume name (in StoragePool) the per-VM
	// overlay disk backs onto.
	Image string
	// StoragePool is the libvirt storage pool the overlay + cloud-init volumes
	// are created in.
	StoragePool string
	// Network is the libvirt network the domain NIC attaches to.
	Network string
	// OverlayGiB caps the overlay disk's logical capacity (thin; only written
	// blocks consume space).
	OverlayGiB int64
}

func (c *libvirtRealConfig) withDefaults() {
	if c.StoragePool == "" {
		c.StoragePool = "default"
	}
	if c.Network == "" {
		c.Network = "default"
	}
	if c.OverlayGiB <= 0 {
		c.OverlayGiB = 40
	}
}

// hostConnection is one zone's live libvirt connection. go-libvirt multiplexes
// concurrent calls over a single connection safely (each request carries a
// serial and responses are demuxed under its own lock), and the fields here are
// set once at construction and never mutated — so no extra mutex is needed, and
// adding one would serialise every op on the host behind a slow multi-minute
// Configure/Drain poll loop (head-of-line blocking).
type hostConnection struct {
	zone string
	uri  string
	lv   *libvirt.Libvirt
}

// libvirtReal is the production libvirtClient, backed by the pure-Go go-libvirt
// (CGO-free, so the distroless/static image builds). Inventory and bindings are
// recovered from each domain's bigfleet metadata element; the cluster-specific
// bootstrap is delivered by writing the opaque blob into the guest and running
// the in-image bootstrap hook via the qemu guest agent (guest-exec), waiting for
// it to complete.
type libvirtReal struct {
	cfg    libvirtRealConfig
	logger *slog.Logger
	conns  map[string]*hostConnection // zone -> connection
}

func newLibvirtReal(cfg libvirtRealConfig, logger *slog.Logger) (*libvirtReal, error) {
	cfg.withDefaults()
	if len(cfg.Connections) == 0 {
		return nil, fmt.Errorf("libvirt: no --connect host connections configured")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("libvirt: --image (golden base volume) is required for the libvirt backend")
	}
	r := &libvirtReal{cfg: cfg, logger: logger, conns: make(map[string]*hostConnection, len(cfg.Connections))}
	for _, hc := range cfg.Connections {
		if _, dup := r.conns[hc.Zone]; dup {
			return nil, fmt.Errorf("libvirt: duplicate zone %q in connections", hc.Zone)
		}
		conn, err := dialLibvirt(hc.URI)
		if err != nil {
			return nil, fmt.Errorf("libvirt: connect zone %q (%s): %w", hc.Zone, hc.URI, err)
		}
		r.conns[hc.Zone] = &hostConnection{zone: hc.Zone, uri: hc.URI, lv: conn}
	}
	return r, nil
}

// dialLibvirt connects to a libvirt URI (qemu:///system, qemu+libssh://…,
// qemu+tls://…) using go-libvirt's URI dialer. Use the qemu+libssh:// scheme for
// SSH — the pinned go-libvirt accepts the keyfile/known_hosts URI parameters
// only on the libssh transport, not on plain qemu+ssh://.
func dialLibvirt(uri string) (*libvirt.Libvirt, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("parse libvirt uri %q: %w", uri, err)
	}
	lv, err := libvirt.ConnectToURI(u)
	if err != nil {
		return nil, err
	}
	return lv, nil
}

func (r *libvirtReal) conn(zone string) (*hostConnection, error) {
	c, ok := r.conns[zone]
	if !ok {
		return nil, fmt.Errorf("no libvirt connection configured for zone %q", zone)
	}
	return c, nil
}

func (r *libvirtReal) CreateDomain(ctx context.Context, spec domainSpec) (domainInstance, error) {
	c, err := r.conn(spec.Zone)
	if err != nil {
		return domainInstance{}, err
	}
	// go-libvirt's generated RPCs block with no context support, so run the
	// create off-goroutine and return promptly when the kit's transition ctx is
	// cancelled (timeout). The abandoned call finishes in the background; the
	// connection is concurrency-safe, so it cannot wedge later ops.
	return callCtx(ctx, func() (domainInstance, error) { return r.createDomain(c, spec) })
}

func (r *libvirtReal) createDomain(c *hostConnection, spec domainSpec) (_ domainInstance, retErr error) {
	name := domainName(spec.IdempotencyToken)
	if spec.IdempotencyToken == "" {
		name = domainName(spec.MachineID)
	}

	// Idempotent define: a retried Create with the same operation id maps to the
	// same domain name; if it already exists, recover it instead of failing. Only
	// a genuine "no such domain" means we should go on to create — any other
	// lookup error (RPC/connection failure) leaves the precondition inconclusive,
	// so fail fast rather than provisioning volumes on a shaky connection.
	existing, err := c.lv.DomainLookupByName(name)
	if err == nil {
		return r.domainView(c, existing)
	}
	if !libvirt.IsNotFound(err) {
		return domainInstance{}, fmt.Errorf("look up domain %s: %w", name, err)
	}

	// Also idempotent on the machine id: a previous Create the kit timed out may
	// have actually completed under a different operation id (hence a different
	// name) — recover that domain rather than launch a duplicate for the same
	// machine.
	if dom, ok, err := r.findByMachineID(c, spec.MachineID); err != nil {
		return domainInstance{}, err
	} else if ok {
		return r.domainView(c, dom)
	}

	pool, err := c.lv.StoragePoolLookupByName(r.cfg.StoragePool)
	if err != nil {
		return domainInstance{}, fmt.Errorf("look up storage pool %q: %w", r.cfg.StoragePool, err)
	}
	base, err := c.lv.StorageVolLookupByName(pool, r.cfg.Image)
	if err != nil {
		return domainInstance{}, fmt.Errorf("look up base image volume %q: %w", r.cfg.Image, err)
	}
	basePath, err := c.lv.StorageVolGetPath(base)
	if err != nil {
		return domainInstance{}, fmt.Errorf("base image path: %w", err)
	}
	// The overlay must be at least the base image's virtual size, or qemu rejects
	// it — so size it to max(base virtual size, configured floor) rather than a
	// fixed default that could be smaller than the golden image.
	_, baseCapacity, _, err := c.lv.StorageVolGetInfo(base)
	if err != nil {
		return domainInstance{}, fmt.Errorf("base image info: %w", err)
	}
	overlayBytes := baseCapacity
	if floor := uint64(r.cfg.OverlayGiB) * 1024 * 1024 * 1024; overlayBytes < floor {
		overlayBytes = floor
	}

	// From here on we create overlay/seed volumes and define the domain. If a
	// later step fails before the domain is started, roll back what we made so a
	// repeated Create does not leak storage or leave partial artifacts. Disarmed
	// (committed) once DomainCreate succeeds — the running domain then owns them.
	committed := false
	var definedDom *libvirt.Domain
	defer func() {
		if retErr == nil || committed {
			return
		}
		if definedDom != nil {
			_ = c.lv.DomainUndefineFlags(*definedDom, libvirt.DomainUndefineManagedSave|libvirt.DomainUndefineNvram)
		}
		r.deleteVolumes(c, name)
	}()

	// Copy-on-write overlay disk backed by the golden base image.
	overlayName := name + "-overlay.qcow2"
	overlay, err := c.lv.StorageVolCreateXML(pool, overlayVolumeXML(overlayName, basePath, overlayBytes), 0)
	if err != nil {
		return domainInstance{}, fmt.Errorf("create overlay volume: %w", err)
	}
	overlayPath, err := c.lv.StorageVolGetPath(overlay)
	if err != nil {
		return domainInstance{}, fmt.Errorf("overlay path: %w", err)
	}

	// cloud-init NoCloud seed (pre-binding user-data; the cluster bootstrap is
	// delivered later by ApplyBootstrap).
	seedPath, err := r.writeSeed(c, pool, name, spec.MachineID, spec.BaseUserData)
	if err != nil {
		return domainInstance{}, err
	}

	meta, err := newBigfleetMeta(spec.MachineID, "").marshal()
	if err != nil {
		return domainInstance{}, err
	}
	xmlDef := renderDomainXML(domainParams{
		Name:      name,
		VCPUs:     spec.VCPUs,
		MemoryMiB: spec.MemoryMiB,
		DiskPath:  overlayPath,
		SeedPath:  seedPath,
		Network:   r.cfg.Network,
		Metadata:  meta,
	})
	dom, err := c.lv.DomainDefineXML(xmlDef)
	if err != nil {
		return domainInstance{}, fmt.Errorf("define domain %s: %w", name, err)
	}
	definedDom = &dom
	if err := c.lv.DomainCreate(dom); err != nil {
		return domainInstance{}, fmt.Errorf("start domain %s: %w", name, err)
	}
	// The domain is defined and running; it now owns the overlay/seed volumes, so
	// stop the rollback. A later domainView error must not tear down a live domain.
	committed = true
	return r.domainView(c, dom)
}

// writeSeed creates a raw volume and uploads the cloud-init NoCloud ISO into it,
// returning the volume path for the domain's CD-ROM.
func (r *libvirtReal) writeSeed(c *hostConnection, pool libvirt.StoragePool, name, machineID string, userData []byte) (string, error) {
	iso, err := buildNoCloudISO(machineID, name, userData)
	if err != nil {
		return "", err
	}
	seedName := name + "-cidata.iso"
	seed, err := c.lv.StorageVolCreateXML(pool, seedVolumeXML(seedName, int64(len(iso))), 0)
	if err != nil {
		return "", fmt.Errorf("create cloud-init volume: %w", err)
	}
	if err := c.lv.StorageVolUpload(seed, bytes.NewReader(iso), 0, uint64(len(iso)), 0); err != nil {
		return "", fmt.Errorf("upload cloud-init seed: %w", err)
	}
	path, err := c.lv.StorageVolGetPath(seed)
	if err != nil {
		return "", fmt.Errorf("cloud-init seed path: %w", err)
	}
	return path, nil
}

func (r *libvirtReal) DeleteDomain(ctx context.Context, zone, name string) error {
	c, err := r.conn(zone)
	if err != nil {
		return err
	}
	return callCtxErr(ctx, func() error { return r.deleteDomain(c, name) })
}

func (r *libvirtReal) deleteDomain(c *hostConnection, name string) error {
	dom, err := c.lv.DomainLookupByName(name)
	if err != nil {
		// Only a genuine "no such domain" is the idempotent already-gone case.
		// Any other error (RPC failure, dropped connection, libvirtd restart) must
		// be reported — swallowing it would settle the machine deleted while the VM
		// is still defined and running, leaking it.
		if libvirt.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("look up domain %s: %w", name, err)
	}
	// Destroy the running domain (ignore "not running"), then undefine removing
	// managed-save + NVRAM state.
	_ = c.lv.DomainDestroy(dom)
	if err := c.lv.DomainUndefineFlags(dom, libvirt.DomainUndefineManagedSave|libvirt.DomainUndefineNvram); err != nil {
		// Fall back to plain undefine for hypervisors that reject the flags;
		// report the fallback's own failure, not the flagged call's.
		if uerr := c.lv.DomainUndefine(dom); uerr != nil {
			return fmt.Errorf("undefine domain %s: %w", name, uerr)
		}
	}
	// Best-effort overlay + seed volume cleanup (keep the golden base image).
	r.deleteVolumes(c, name)
	return nil
}

// findByMachineID returns the managed domain tagged with the given machine id,
// if any (used to make Create idempotent on the machine id, not just the
// operation-id-derived name).
func (r *libvirtReal) findByMachineID(c *hostConnection, machineID string) (libvirt.Domain, bool, error) {
	domains, _, err := c.lv.ConnectListAllDomains(1, libvirt.ConnectListDomainsPersistent)
	if err != nil {
		return libvirt.Domain{}, false, fmt.Errorf("list domains: %w", err)
	}
	for _, dom := range domains {
		if mid, _ := r.readMetadata(c, dom); mid == machineID {
			return dom, true, nil
		}
	}
	return libvirt.Domain{}, false, nil
}

func (r *libvirtReal) deleteVolumes(c *hostConnection, name string) {
	pool, err := c.lv.StoragePoolLookupByName(r.cfg.StoragePool)
	if err != nil {
		return
	}
	for _, vol := range []string{name + "-overlay.qcow2", name + "-cidata.iso"} {
		if v, err := c.lv.StorageVolLookupByName(pool, vol); err == nil {
			if derr := c.lv.StorageVolDelete(v, 0); derr != nil && r.logger != nil {
				r.logger.Warn("delete volume", "volume", vol, "err", derr)
			}
		}
	}
}

func (r *libvirtReal) DescribeManaged(_ context.Context) ([]domainInstance, error) {
	var out []domainInstance
	for zone, c := range r.conns {
		domains, _, err := c.lv.ConnectListAllDomains(1, libvirt.ConnectListDomainsPersistent)
		if err != nil {
			return nil, fmt.Errorf("list domains in zone %q: %w", zone, err)
		}
		for _, dom := range domains {
			// Skip non-bigfleet domains before probing state, so an unrelated
			// VM's transient state blip can't fail our reconcile.
			if mid, _ := r.readMetadata(c, dom); mid == "" {
				continue
			}
			view, err := r.domainView(c, dom)
			if err != nil {
				return nil, fmt.Errorf("inspect managed domain %s in zone %q: %w", dom.Name, zone, err)
			}
			out = append(out, view)
		}
	}
	return out, nil
}

func (r *libvirtReal) ApplyBootstrap(ctx context.Context, dom domainInstance, clusterID string, bootstrap []byte) error {
	c, err := r.conn(dom.Zone)
	if err != nil {
		return err
	}
	// Bound the whole sequence (the outer lookup/setMetadata RPCs as well as the
	// guest-exec loop) by the transition ctx, like Create/Delete.
	return callCtxErr(ctx, func() error { return r.applyBootstrap(ctx, c, dom, clusterID, bootstrap) })
}

func (r *libvirtReal) applyBootstrap(ctx context.Context, c *hostConnection, dom domainInstance, clusterID string, bootstrap []byte) error {
	d, err := c.lv.DomainLookupByName(dom.DomainName)
	if err != nil {
		return fmt.Errorf("configure: look up domain %s: %w", dom.DomainName, err)
	}
	// Deliver the opaque bootstrap blob to the guest via the qemu guest agent
	// (write the blob to a file, then run the in-image bootstrap hook). The guest
	// must run qemu-guest-agent and ship the hook; we wait for it to succeed so a
	// failed bootstrap surfaces as FAILED.
	if err := r.guestWriteAndRun(ctx, c, d, bootstrap, clusterID); err != nil {
		return err
	}
	// Record the binding in the domain metadata only AFTER the bootstrap
	// succeeded, so a failed Configure never leaves a domain tagged as bound to a
	// cluster it never joined.
	return r.setMetadata(c, d, dom.MachineID, clusterID)
}

func (r *libvirtReal) DrainNode(ctx context.Context, dom domainInstance, gracePeriodSeconds int64) error {
	c, err := r.conn(dom.Zone)
	if err != nil {
		return err
	}
	return callCtxErr(ctx, func() error { return r.drainNode(ctx, c, dom, gracePeriodSeconds) })
}

func (r *libvirtReal) drainNode(ctx context.Context, c *hostConnection, dom domainInstance, gracePeriodSeconds int64) error {
	d, err := c.lv.DomainLookupByName(dom.DomainName)
	if err != nil {
		return fmt.Errorf("drain: look up domain %s: %w", dom.DomainName, err)
	}
	grace := gracePeriodSeconds
	if grace <= 0 {
		grace = 1
	}
	// Cordon + drain the kubelet via the guest agent, bounded by the grace period.
	script := fmt.Sprintf(
		"set -e; node=$(hostname -f 2>/dev/null || hostname); "+
			"kubectl cordon \"$node\" || true; "+
			"kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds",
		grace, grace)
	if err := r.guestExec(ctx, c, d, script); err != nil {
		return err
	}
	return r.setMetadata(c, d, dom.MachineID, "")
}

func (r *libvirtReal) Close() error {
	var firstErr error
	for _, c := range r.conns {
		if c.lv != nil {
			if err := c.lv.Disconnect(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// --- helpers --------------------------------------------------------------

// domainView builds the substrate view of a domain from its libvirt identity +
// bigfleet metadata. A DomainGetState failure is surfaced (not swallowed into
// running=false), so a transient RPC error never mislabels a live domain as
// not-running.
func (r *libvirtReal) domainView(c *hostConnection, dom libvirt.Domain) (domainInstance, error) {
	machineID, clusterID := r.readMetadata(c, dom)
	state, _, err := c.lv.DomainGetState(dom, 0)
	if err != nil {
		return domainInstance{}, fmt.Errorf("get state of domain %s: %w", dom.Name, err)
	}
	return domainInstance{
		Zone:       c.zone,
		DomainName: dom.Name,
		UUID:       uuidString(dom.UUID),
		MachineID:  machineID,
		ClusterID:  clusterID,
		Running:    libvirt.DomainState(state) == libvirt.DomainRunning,
	}, nil
}

// readMetadata returns the (machineID, clusterID) from a domain's bigfleet
// metadata element, or empty strings if absent.
func (r *libvirtReal) readMetadata(c *hostConnection, dom libvirt.Domain) (string, string) {
	raw, err := c.lv.DomainGetMetadata(dom, int32(libvirt.DomainMetadataElement), optString(bigfleetMetadataNS), libvirt.DomainAffectConfig)
	if err != nil || raw == "" {
		return "", ""
	}
	var meta struct {
		MachineID string `xml:"machineID"`
		ClusterID string `xml:"clusterID"`
	}
	if err := xml.Unmarshal([]byte(raw), &meta); err != nil {
		return "", ""
	}
	return meta.MachineID, meta.ClusterID
}

// setMetadata writes the bigfleet metadata element on a domain.
func (r *libvirtReal) setMetadata(c *hostConnection, dom libvirt.Domain, machineID, clusterID string) error {
	meta, err := newBigfleetMeta(machineID, clusterID).marshal()
	if err != nil {
		return err
	}
	return c.lv.DomainSetMetadata(dom, int32(libvirt.DomainMetadataElement),
		optString(meta), optString(bigfleetMetadataKey), optString(bigfleetMetadataNS),
		libvirt.DomainAffectConfig|libvirt.DomainAffectLive)
}

// guestWriteAndRun writes the bootstrap blob to a file in the guest and runs the
// in-image bootstrap hook with the cluster id, via the qemu guest agent. The
// blob is opaque, so it is delivered base64-encoded (never parsed) and the
// in-image hook decodes + applies it. The blob file is the cluster-join secret,
// so an EXIT trap shreds/removes it whether the hook succeeds or fails — it is
// never left cleartext-at-rest in the guest.
func (r *libvirtReal) guestWriteAndRun(ctx context.Context, c *hostConnection, dom libvirt.Domain, blob []byte, clusterID string) error {
	script := fmt.Sprintf(
		"set -e; umask 077; mkdir -p /opt/bigfleet; "+
			"trap 'rm -f /opt/bigfleet/bootstrap.blob' EXIT; "+
			"printf '%%s' %q | base64 -d > /opt/bigfleet/bootstrap.blob; "+
			"/opt/bigfleet/bootstrap %q",
		base64.StdEncoding.EncodeToString(blob), clusterID)
	return r.guestExec(ctx, c, dom, script)
}

// guestExecPollInterval is how often guestExec polls guest-exec-status for
// completion. The transition timeout (carried on ctx) is the real bound.
const guestExecPollInterval = 2 * time.Second

// guestAgentPingInterval is how often waitGuestAgentReady retries guest-ping
// while the guest finishes booting (cloud-init + qemu-guest-agent start).
const guestAgentPingInterval = 3 * time.Second

// guestExec runs a shell command in the guest via the qemu guest agent and waits
// for it to ACTUALLY complete. It first waits for the guest agent to come online
// (Create settles a machine Idle as soon as QEMU is running, which can be well
// before cloud-init/qemu-guest-agent are up, so a prompt Configure/Drain must not
// treat an agent-not-ready error as a hard failure). guest-exec is then
// asynchronous — it returns a pid — so we poll guest-exec-status until the
// process exits and check its exit code. A non-zero exit (agent error, or ctx
// cancellation when the transition times out) returns an error so the transition
// surfaces as FAILED.
func (r *libvirtReal) guestExec(ctx context.Context, c *hostConnection, dom libvirt.Domain, script string) error {
	if err := r.waitGuestAgentReady(ctx, c, dom); err != nil {
		return err
	}
	cmd := fmt.Sprintf(`{"execute":"guest-exec","arguments":{"path":"/bin/sh","arg":["-c",%q],"capture-output":true}}`, script)
	raw, err := c.lv.QEMUDomainAgentCommand(dom, cmd, 60, 0)
	if err != nil {
		return fmt.Errorf("guest agent exec: %w", err)
	}
	pid, err := parseGuestExecPID(optStringValue(raw))
	if err != nil {
		return fmt.Errorf("guest agent exec: %w", err)
	}

	statusCmd := fmt.Sprintf(`{"execute":"guest-exec-status","arguments":{"pid":%d}}`, pid)
	for {
		sraw, err := c.lv.QEMUDomainAgentCommand(dom, statusCmd, 60, 0)
		if err != nil {
			return fmt.Errorf("guest agent exec-status (pid %d): %w", pid, err)
		}
		st, err := parseGuestExecStatus(optStringValue(sraw))
		if err != nil {
			return fmt.Errorf("guest agent exec-status (pid %d): %w", pid, err)
		}
		if st.Exited {
			if st.ExitCode != 0 {
				return fmt.Errorf("guest command exited %d: %s", st.ExitCode, decodeAgentData(st.ErrData))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest command (pid %d) did not complete: %w", pid, ctx.Err())
		case <-time.After(guestExecPollInterval):
		}
	}
}

// guestExecStatus is the subset of a qemu guest-exec-status reply we act on.
type guestExecStatus struct {
	Exited   bool   `json:"exited"`
	ExitCode int    `json:"exitcode"`
	ErrData  string `json:"err-data"` // base64, present only when capture-output was set
}

// parseGuestExecPID extracts the pid from a guest-exec reply
// ({"return":{"pid":N}}).
func parseGuestExecPID(jsonReply string) (int, error) {
	var resp struct {
		Return struct {
			PID int `json:"pid"`
		} `json:"return"`
	}
	if err := json.Unmarshal([]byte(jsonReply), &resp); err != nil {
		return 0, fmt.Errorf("parse guest-exec reply %q: %w", jsonReply, err)
	}
	if resp.Return.PID == 0 {
		return 0, fmt.Errorf("guest-exec reply has no pid: %q", jsonReply)
	}
	return resp.Return.PID, nil
}

// parseGuestExecStatus extracts the status from a guest-exec-status reply
// ({"return":{"exited":bool,"exitcode":N,"err-data":"..."}}).
func parseGuestExecStatus(jsonReply string) (guestExecStatus, error) {
	var resp struct {
		Return guestExecStatus `json:"return"`
	}
	if err := json.Unmarshal([]byte(jsonReply), &resp); err != nil {
		return guestExecStatus{}, fmt.Errorf("parse guest-exec-status reply %q: %w", jsonReply, err)
	}
	return resp.Return, nil
}

// decodeAgentData best-effort base64-decodes captured guest stderr for error
// messages; returns the raw string if it is not valid base64.
func decodeAgentData(b64 string) string {
	if b64 == "" {
		return ""
	}
	if dec, err := base64.StdEncoding.DecodeString(b64); err == nil {
		return strings.TrimSpace(string(dec))
	}
	return b64
}

// waitGuestAgentReady blocks until the qemu guest agent answers a guest-ping, or
// ctx (the transition timeout) expires. This absorbs the boot / cloud-init window
// after Create settles a machine Idle (which happens as soon as QEMU is running,
// before the in-guest agent is up), so a prompt Configure/Drain does not fail
// just because the agent has not started answering yet.
func (r *libvirtReal) waitGuestAgentReady(ctx context.Context, c *hostConnection, dom libvirt.Domain) error {
	const ping = `{"execute":"guest-ping"}`
	for {
		if _, err := c.lv.QEMUDomainAgentCommand(dom, ping, 5, 0); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("qemu guest agent on %s not ready: %w", dom.Name, ctx.Err())
		case <-time.After(guestAgentPingInterval):
		}
	}
}

// callCtx runs fn — a blocking sequence of go-libvirt RPCs, which have no native
// context support — and returns as soon as ctx is cancelled (e.g. the kit's
// transition timeout fires). On cancellation the worker goroutine keeps running
// until its in-flight RPC returns; go-libvirt multiplexes it safely over the
// shared connection, so an abandoned call cannot wedge later operations.
func callCtx[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case res := <-ch:
		return res.v, res.err
	}
}

// callCtxErr is callCtx for a function that returns only an error.
func callCtxErr(ctx context.Context, fn func() error) error {
	_, err := callCtx(ctx, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// optStringValue returns the value carried by a go-libvirt OptString (an
// optional field encoded as a 0- or 1-element slice), or "" when unset.
func optStringValue(o libvirt.OptString) string {
	if len(o) == 0 {
		return ""
	}
	return o[0]
}

func optString(s string) libvirt.OptString {
	if s == "" {
		return libvirt.OptString{}
	}
	return libvirt.OptString{s}
}

func uuidString(u libvirt.UUID) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

var _ libvirtClient = (*libvirtReal)(nil)
