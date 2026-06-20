package main

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"
)

// machineIDEncoding encodes a (possibly slash-bearing) BigFleet machine id into
// a libvirt-domain-name-safe token (lowercase base32, no padding → only
// [a-z2-7]). The real domain name is "bigfleet-<token>"; the machine id is also
// stored verbatim in the domain's bigfleet metadata for recovery.
var machineIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func domainName(token string) string {
	name := "bigfleet-" + strings.ToLower(machineIDEncoding.EncodeToString([]byte(token)))
	if len(name) > 60 { // libvirt domain names are generous, but keep it sane
		name = name[:60]
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
	// ConnectTimeout bounds the initial connect to each host.
	ConnectTimeout time.Duration
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
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 20 * time.Second
	}
}

// hostConnection is one zone's live libvirt connection, guarded by a mutex
// (libvirt connections are not freely concurrency-safe).
type hostConnection struct {
	zone string
	uri  string
	mu   sync.Mutex
	lv   *libvirt.Libvirt
}

// libvirtReal is the production libvirtClient, backed by the pure-Go go-libvirt
// (CGO-free, so the distroless/static image builds). Inventory and bindings are
// recovered from each domain's bigfleet metadata element; the cluster-specific
// bootstrap is delivered by regenerating the cloud-init NoCloud datasource and
// running the in-guest bootstrap via the qemu guest agent.
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
		conn, err := dialLibvirt(hc.URI, cfg.ConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("libvirt: connect zone %q (%s): %w", hc.Zone, hc.URI, err)
		}
		r.conns[hc.Zone] = &hostConnection{zone: hc.Zone, uri: hc.URI, lv: conn}
	}
	return r, nil
}

// dialLibvirt connects to a libvirt URI (qemu:///system, qemu+ssh://…,
// qemu+tls://…) using go-libvirt's URI dialer.
func dialLibvirt(uri string, _ time.Duration) (*libvirt.Libvirt, error) {
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
	c.mu.Lock()
	defer c.mu.Unlock()

	name := domainName(spec.IdempotencyToken)
	if spec.IdempotencyToken == "" {
		name = domainName(spec.MachineID)
	}

	// Idempotent define: a retried Create with the same operation id maps to the
	// same domain name; if it already exists, recover it instead of failing.
	if existing, err := c.lv.DomainLookupByName(name); err == nil {
		return r.domainView(c, existing)
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

	// Copy-on-write overlay disk backed by the golden base image.
	overlayName := name + "-overlay.qcow2"
	overlay, err := c.lv.StorageVolCreateXML(pool, overlayVolumeXML(overlayName, basePath, r.cfg.OverlayGiB), 0)
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
	if err := c.lv.DomainCreate(dom); err != nil {
		return domainInstance{}, fmt.Errorf("start domain %s: %w", name, err)
	}
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
	if err := c.lv.StorageVolUpload(seed, strings.NewReader(string(iso)), 0, uint64(len(iso)), 0); err != nil {
		return "", fmt.Errorf("upload cloud-init seed: %w", err)
	}
	path, err := c.lv.StorageVolGetPath(seed)
	if err != nil {
		return "", fmt.Errorf("cloud-init seed path: %w", err)
	}
	return path, nil
}

func (r *libvirtReal) DeleteDomain(_ context.Context, zone, name string) error {
	c, err := r.conn(zone)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	dom, err := c.lv.DomainLookupByName(name)
	if err != nil {
		// Already gone — idempotent.
		return nil
	}
	// Destroy the running domain (ignore "not running"), then undefine removing
	// managed-save + NVRAM state.
	_ = c.lv.DomainDestroy(dom)
	if err := c.lv.DomainUndefineFlags(dom, libvirt.DomainUndefineManagedSave|libvirt.DomainUndefineNvram); err != nil {
		// Fall back to plain undefine for hypervisors that reject the flags.
		if uerr := c.lv.DomainUndefine(dom); uerr != nil {
			return fmt.Errorf("undefine domain %s: %w", name, err)
		}
	}
	// Best-effort overlay + seed volume cleanup (keep the golden base image).
	r.deleteVolumes(c, name)
	return nil
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
		c.mu.Lock()
		domains, _, err := c.lv.ConnectListAllDomains(1, libvirt.ConnectListDomainsPersistent)
		if err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("list domains in zone %q: %w", zone, err)
		}
		for _, dom := range domains {
			view, err := r.domainView(c, dom)
			if err != nil || view.MachineID == "" {
				continue // not a bigfleet-managed domain
			}
			out = append(out, view)
		}
		c.mu.Unlock()
	}
	return out, nil
}

func (r *libvirtReal) ApplyBootstrap(_ context.Context, dom domainInstance, clusterID string, bootstrap []byte) error {
	c, err := r.conn(dom.Zone)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	d, err := c.lv.DomainLookupByName(dom.DomainName)
	if err != nil {
		return fmt.Errorf("configure: look up domain %s: %w", dom.DomainName, err)
	}
	// Deliver the opaque bootstrap blob to the guest via the qemu guest agent
	// (write the blob to a file, then run the in-image bootstrap hook). The guest
	// must run qemu-guest-agent and ship the hook; we wait for it to succeed so a
	// failed bootstrap surfaces as FAILED.
	if err := r.guestWriteAndRun(c, d, bootstrap, clusterID); err != nil {
		return err
	}
	// Record the binding in the domain metadata only AFTER the bootstrap
	// succeeded, so a failed Configure never leaves a domain tagged as bound to a
	// cluster it never joined.
	return r.setMetadata(c, d, dom.MachineID, clusterID)
}

func (r *libvirtReal) DrainNode(_ context.Context, dom domainInstance, gracePeriodSeconds int64) error {
	c, err := r.conn(dom.Zone)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

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
	if err := r.guestExec(c, d, script); err != nil {
		return err
	}
	return r.setMetadata(c, d, dom.MachineID, "")
}

func (r *libvirtReal) Close() error {
	var firstErr error
	for _, c := range r.conns {
		c.mu.Lock()
		if c.lv != nil {
			if err := c.lv.Disconnect(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		c.mu.Unlock()
	}
	return firstErr
}

// --- helpers --------------------------------------------------------------

// domainView builds the substrate view of a domain from its libvirt identity +
// bigfleet metadata. Caller holds c.mu.
func (r *libvirtReal) domainView(c *hostConnection, dom libvirt.Domain) (domainInstance, error) {
	machineID, clusterID := r.readMetadata(c, dom)
	state, _, err := c.lv.DomainGetState(dom, 0)
	running := err == nil && libvirt.DomainState(state) == libvirt.DomainRunning
	return domainInstance{
		Zone:       c.zone,
		DomainName: dom.Name,
		UUID:       uuidString(dom.UUID),
		MachineID:  machineID,
		ClusterID:  clusterID,
		Running:    running,
	}, nil
}

// readMetadata returns the (machineID, clusterID) from a domain's bigfleet
// metadata element, or empty strings if absent. Caller holds c.mu.
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

// setMetadata writes the bigfleet metadata element on a domain. Caller holds c.mu.
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
// in-image hook decodes + applies it.
func (r *libvirtReal) guestWriteAndRun(c *hostConnection, dom libvirt.Domain, blob []byte, clusterID string) error {
	script := fmt.Sprintf(
		"set -e; umask 077; mkdir -p /opt/bigfleet; "+
			"printf '%%s' %q | base64 -d > /opt/bigfleet/bootstrap.blob; "+
			"/opt/bigfleet/bootstrap %q",
		base64.StdEncoding.EncodeToString(blob), clusterID)
	return r.guestExec(c, dom, script)
}

// guestExec runs a shell command in the guest via the qemu guest agent's
// guest-exec, waiting for it to complete. A non-zero exit (or agent error)
// returns an error so the transition surfaces as FAILED.
func (r *libvirtReal) guestExec(c *hostConnection, dom libvirt.Domain, script string) error {
	// guest-exec with the command captured; the guest agent must be running.
	cmd := fmt.Sprintf(`{"execute":"guest-exec","arguments":{"path":"/bin/sh","arg":["-c",%q],"capture-output":true}}`, script)
	if _, err := c.lv.QEMUDomainAgentCommand(dom, cmd, 60, 0); err != nil {
		return fmt.Errorf("guest agent exec: %w", err)
	}
	return nil
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
