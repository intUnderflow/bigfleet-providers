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
	"errors"
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
		return r.recoverDomain(c, existing)
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
		return r.recoverDomain(c, dom)
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
	//
	// Rollback deletes only the volumes THIS attempt actually created (createdVols),
	// never ones it adopted: a retried Create under the same idempotency token can
	// race an abandoned earlier worker that already created the same-named volumes
	// and is still using them — deleting those would pull the disk out from under a
	// live (or about-to-be-defined) domain.
	committed := false
	var createdVols []string
	var definedDom *libvirt.Domain
	defer func() {
		if retErr == nil || committed {
			return
		}
		if definedDom != nil {
			_ = r.undefineDomain(c, *definedDom)
		}
		r.deleteNamedVolumes(c, createdVols)
	}()

	// Copy-on-write overlay disk backed by the golden base image.
	overlayName := name + "-overlay.qcow2"
	overlay, created, err := r.createOrAdoptVol(c, pool, overlayName, overlayVolumeXML(overlayName, basePath, overlayBytes))
	if created {
		createdVols = append(createdVols, overlayName)
	}
	if err != nil {
		return domainInstance{}, fmt.Errorf("create overlay volume: %w", err)
	}
	overlayPath, err := c.lv.StorageVolGetPath(overlay)
	if err != nil {
		return domainInstance{}, fmt.Errorf("overlay path: %w", err)
	}

	// cloud-init NoCloud seed (pre-binding user-data; the cluster bootstrap is
	// delivered later by ApplyBootstrap).
	seedPath, seedCreated, err := r.writeSeed(c, pool, name, spec.MachineID, spec.BaseUserData)
	if seedCreated {
		createdVols = append(createdVols, name+"-cidata.iso")
	}
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
	// Autostart so the node powers back on after a host reboot rather than sitting
	// persistent-but-shut-off (which would otherwise be advertised Idle yet be
	// unreachable). Best-effort: a hypervisor that rejects it doesn't fail Create.
	if err := c.lv.DomainSetAutostart(dom, 1); err != nil && r.logger != nil {
		r.logger.Warn("set domain autostart", "domain", name, "err", err)
	}
	// The domain is defined and running; it now owns the overlay/seed volumes, so
	// stop the rollback. A later domainView error must not tear down a live domain.
	committed = true
	return r.domainView(c, dom)
}

// recoverDomain returns the substrate view of an existing domain found on a
// Create idempotency-recovery branch, first ensuring it is actually powered on:
// a recovered domain may be persistent-but-shut-off (host reboot without
// autostart, or an in-guest poweroff), and the kit settles it Idle, so the shard
// would schedule onto a dead node. A shut-off domain is started; a paused one is
// resumed; autostart is re-asserted.
func (r *libvirtReal) recoverDomain(c *hostConnection, dom libvirt.Domain) (domainInstance, error) {
	state, _, err := c.lv.DomainGetState(dom, 0)
	if err != nil {
		return domainInstance{}, fmt.Errorf("get state of domain %s: %w", dom.Name, err)
	}
	switch libvirt.DomainState(state) {
	case libvirt.DomainShutoff, libvirt.DomainCrashed:
		if err := c.lv.DomainCreate(dom); err != nil {
			return domainInstance{}, fmt.Errorf("start recovered domain %s: %w", dom.Name, err)
		}
	case libvirt.DomainPaused, libvirt.DomainPmsuspended:
		if err := c.lv.DomainResume(dom); err != nil {
			return domainInstance{}, fmt.Errorf("resume recovered domain %s: %w", dom.Name, err)
		}
	}
	if err := c.lv.DomainSetAutostart(dom, 1); err != nil && r.logger != nil {
		r.logger.Warn("set domain autostart (recovery)", "domain", dom.Name, "err", err)
	}
	return r.domainView(c, dom)
}

// writeSeed creates a raw volume and uploads the cloud-init NoCloud ISO into it,
// returning the volume path for the domain's CD-ROM. The bool reports whether
// this call created the volume (vs adopted a pre-existing one) so the caller's
// rollback only removes volumes it owns. The ISO content is deterministic from
// (machineID, name, userData), so re-uploading into an adopted volume is safe.
func (r *libvirtReal) writeSeed(c *hostConnection, pool libvirt.StoragePool, name, machineID string, userData []byte) (string, bool, error) {
	iso, err := buildNoCloudISO(machineID, name, userData)
	if err != nil {
		return "", false, err
	}
	seedName := name + "-cidata.iso"
	seed, created, err := r.createOrAdoptVol(c, pool, seedName, seedVolumeXML(seedName, int64(len(iso))))
	if err != nil {
		return "", false, fmt.Errorf("create cloud-init volume: %w", err)
	}
	if err := c.lv.StorageVolUpload(seed, bytes.NewReader(iso), 0, uint64(len(iso)), 0); err != nil {
		return "", created, fmt.Errorf("upload cloud-init seed: %w", err)
	}
	path, err := c.lv.StorageVolGetPath(seed)
	if err != nil {
		return "", created, fmt.Errorf("cloud-init seed path: %w", err)
	}
	return path, created, nil
}

// createOrAdoptVol creates a storage volume, treating ERR_STORAGE_VOL_EXIST (a
// volume of the same name already present — e.g. a concurrent retried Create
// under the same idempotency token) as the idempotent already-there case: it
// looks the existing volume up and reports created=false so the caller's
// rollback won't delete a volume another in-flight attempt is still using.
func (r *libvirtReal) createOrAdoptVol(c *hostConnection, pool libvirt.StoragePool, name, xml string) (libvirt.StorageVol, bool, error) {
	vol, err := c.lv.StorageVolCreateXML(pool, xml, 0)
	if err == nil {
		return vol, true, nil
	}
	if !isStorageVolExist(err) {
		return libvirt.StorageVol{}, false, err
	}
	existing, lerr := c.lv.StorageVolLookupByName(pool, name)
	if lerr != nil {
		return libvirt.StorageVol{}, false, fmt.Errorf("look up pre-existing volume %q: %w", name, lerr)
	}
	return existing, false, nil
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
	if err := r.undefineDomain(c, dom); err != nil {
		return fmt.Errorf("undefine domain %s: %w", name, err)
	}
	// Best-effort overlay + seed volume cleanup (keep the golden base image).
	r.deleteVolumes(c, name)
	return nil
}

// undefineDomain undefines a domain, removing managed-save + NVRAM state, with a
// fallback to a plain undefine for hypervisors/libvirtd that reject the flags.
// Returns the fallback's own error, not the flagged call's.
func (r *libvirtReal) undefineDomain(c *hostConnection, dom libvirt.Domain) error {
	if err := c.lv.DomainUndefineFlags(dom, libvirt.DomainUndefineManagedSave|libvirt.DomainUndefineNvram); err != nil {
		if uerr := c.lv.DomainUndefine(dom); uerr != nil {
			return uerr
		}
	}
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

// deleteVolumes removes the overlay + seed volumes for a committed domain (Delete
// cleanup). It deletes both by name unconditionally because a committed domain
// owns them; the golden base image is left untouched.
func (r *libvirtReal) deleteVolumes(c *hostConnection, name string) {
	r.deleteNamedVolumes(c, []string{name + "-overlay.qcow2", name + "-cidata.iso"})
}

// deleteNamedVolumes best-effort deletes exactly the named volumes from the
// storage pool. Create rollback passes only volumes the failed attempt created,
// so a concurrent retried Create's volumes are never deleted out from under it.
func (r *libvirtReal) deleteNamedVolumes(c *hostConnection, names []string) {
	if len(names) == 0 {
		return
	}
	pool, err := c.lv.StoragePoolLookupByName(r.cfg.StoragePool)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("volume cleanup: storage pool lookup failed", "pool", r.cfg.StoragePool, "err", err)
		}
		return
	}
	for _, vol := range names {
		v, err := c.lv.StorageVolLookupByName(pool, vol)
		if err != nil {
			// An already-gone volume (ERR_NO_STORAGE_VOL) is the expected nothing-
			// to-do case; any other lookup error is logged so a transient failure
			// leaving an orphaned volume is at least visible (cleanup is
			// best-effort and must not fail the surrounding operation).
			if !isStorageVolNotFound(err) && r.logger != nil {
				r.logger.Warn("volume cleanup: lookup failed (possible orphan)", "volume", vol, "err", err)
			}
			continue
		}
		if derr := c.lv.StorageVolDelete(v, 0); derr != nil && r.logger != nil {
			r.logger.Warn("volume cleanup: delete failed", "volume", vol, "err", derr)
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
			// Read metadata once: skip non-bigfleet domains before probing state
			// (so an unrelated VM's transient state blip can't fail our reconcile),
			// and reuse it for the view rather than re-reading it.
			machineID, clusterID := r.readMetadata(c, dom)
			if machineID == "" {
				continue
			}
			view, err := r.domainViewMeta(c, dom, machineID, clusterID)
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
// bigfleet metadata.
func (r *libvirtReal) domainView(c *hostConnection, dom libvirt.Domain) (domainInstance, error) {
	machineID, clusterID := r.readMetadata(c, dom)
	return r.domainViewMeta(c, dom, machineID, clusterID)
}

// domainViewMeta builds the view from already-read metadata (so callers that
// have just read it — e.g. DescribeManaged's managed-domain filter — don't pay a
// second DomainGetMetadata RPC). A DomainGetState failure is surfaced (not
// swallowed into running=false), so a transient RPC error never mislabels a live
// domain as not-running.
func (r *libvirtReal) domainViewMeta(c *hostConnection, dom libvirt.Domain, machineID, clusterID string) (domainInstance, error) {
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
		Running:    domainActive(libvirt.DomainState(state)),
	}, nil
}

// domainActive reports whether a domain is live (not shut off / crashed /
// shutting down). A paused or pm-suspended domain is still a live domain — it is
// not a free slot — so it counts as active.
func domainActive(s libvirt.DomainState) bool {
	switch s {
	case libvirt.DomainRunning, libvirt.DomainBlocked, libvirt.DomainPaused, libvirt.DomainPmsuspended:
		return true
	default: // Nostate, Shutdown (in progress), Shutoff, Crashed
		return false
	}
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
// so an EXIT trap removes it (best-effort `rm -f`, not a secure wipe) whether the
// hook succeeds or fails, so it is not left lying around in the guest afterward.
func (r *libvirtReal) guestWriteAndRun(ctx context.Context, c *hostConnection, dom libvirt.Domain, blob []byte, clusterID string) error {
	// The cluster id is shard-supplied; single-quote it (and the base64 blob) for
	// /bin/sh so it can't break out of or inject into the script. (%q is Go
	// quoting, NOT shell-safe.)
	script := fmt.Sprintf(
		"set -e; umask 077; mkdir -p /opt/bigfleet; "+
			"trap 'rm -f /opt/bigfleet/bootstrap.blob' EXIT; "+
			"printf '%%s' %s | base64 -d > /opt/bigfleet/bootstrap.blob; "+
			"/opt/bigfleet/bootstrap %s",
		shellQuote(base64.StdEncoding.EncodeToString(blob)), shellQuote(clusterID))
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
	// Build the guest-exec request with encoding/json, not fmt %q: the script
	// embeds a shard-supplied clusterID, and %q is Go quoting (e.g. a control byte
	// becomes \a), which is not valid JSON and would make QEMUDomainAgentCommand
	// reject the command. json.Marshal emits proper \uXXXX escapes.
	cmdReq := struct {
		Execute   string `json:"execute"`
		Arguments struct {
			Path          string   `json:"path"`
			Arg           []string `json:"arg"`
			CaptureOutput bool     `json:"capture-output"`
		} `json:"arguments"`
	}{Execute: "guest-exec"}
	cmdReq.Arguments.Path = "/bin/sh"
	cmdReq.Arguments.Arg = []string{"-c", script}
	cmdReq.Arguments.CaptureOutput = true
	cmd, err := json.Marshal(cmdReq)
	if err != nil {
		return fmt.Errorf("marshal guest-exec command: %w", err)
	}
	raw, err := c.lv.QEMUDomainAgentCommand(dom, string(cmd), 60, 0)
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
				return fmt.Errorf("guest command exited %d (stderr: %s) (stdout: %s)",
					st.ExitCode, decodeAgentData(st.ErrData), decodeAgentData(st.OutData))
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
	OutData  string `json:"out-data"` // base64, present only when capture-output was set
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

// isStorageVolNotFound reports whether err is libvirt's ERR_NO_STORAGE_VOL
// (go-libvirt's IsNotFound only covers ERR_NO_DOMAIN).
func isStorageVolNotFound(err error) bool {
	var lerr libvirt.Error
	if errors.As(err, &lerr) {
		return lerr.Code == uint32(libvirt.ErrNoStorageVol)
	}
	return false
}

// isStorageVolExist reports whether err is libvirt's ERR_STORAGE_VOL_EXIST
// (a volume of that name already exists).
func isStorageVolExist(err error) bool {
	var lerr libvirt.Error
	if errors.As(err, &lerr) {
		return lerr.Code == uint32(libvirt.ErrStorageVolExist)
	}
	return false
}

// shellQuote single-quotes a string for safe interpolation into a /bin/sh
// command, so an opaque/shard-supplied value can't break out of the script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var _ libvirtClient = (*libvirtReal)(nil)
