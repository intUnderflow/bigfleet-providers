package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/godo"
)

// BigFleet Droplet tag keys. bigfleet-managed marks our Droplets so
// DescribeManaged never touches anything else; the rest let inventory and
// bindings be recovered from DigitalOcean alone. DigitalOcean tag names allow
// [A-Za-z0-9:_-] up to 255 chars, so the (possibly slash-bearing) machine id and
// cluster id are base32-encoded into the tag suffix and decoded back on read.
const (
	tagManaged       = "bigfleet-managed"
	tagMachinePrefix = "bfmid:"
	tagClusterPrefix = "bfcluster:"
)

// idEncoding encodes an id into a DigitalOcean-tag-safe suffix (lowercase base32
// without padding → only [a-z2-7]). Round-trips comfortably within the 255-char
// tag limit; the FileStore remains the documented primary restart path.
var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func encodeID(id string) string {
	return strings.ToLower(idEncoding.EncodeToString([]byte(id)))
}

func decodeID(suffix string) string {
	b, err := idEncoding.DecodeString(strings.ToUpper(suffix))
	if err != nil {
		return ""
	}
	return string(b)
}

// doRealConfig is the launch configuration for the production DigitalOcean client.
type doRealConfig struct {
	Token  string
	Region string // the region this process serves
	Image  string // base image / snapshot slug or numeric id for Droplets.Create

	// Vault is the on-host agent control channel used by ApplyBootstrap /
	// DrainNode (DigitalOcean has no in-guest command API). Required.
	Vault *bootstrapVault
	// BootstrapEndpoint is the externally-reachable URL of the provider's
	// bootstrap channel (e.g. https://do-provider.example:9443). It is injected
	// into the Droplet's generic user_data so the agent knows where to fetch.
	BootstrapEndpoint string
	// BootstrapCAPEM is the PEM the agent pins to verify the provider's server
	// certificate — the agent side of the mutual authentication.
	BootstrapCAPEM string

	// CreateWaitTimeout caps how long CreateDroplet waits for the Droplet to
	// reach 'active' (the kit's Create timeout, carried on ctx, usually fires
	// first).
	CreateWaitTimeout time.Duration
	// PollInterval is how often CreateDroplet polls the Droplet status.
	PollInterval time.Duration
	// SizesCacheTTL bounds how long a fetched Sizes.List catalogue is reused, so
	// a price-refresh over many sizes is one catalogue scan, not one per size.
	SizesCacheTTL time.Duration
}

func (c *doRealConfig) withDefaults() {
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 10 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.SizesCacheTTL <= 0 {
		c.SizesCacheTTL = time.Minute
	}
}

// doReal is the production doClient, backed by godo. Inventory and bindings are
// recovered from Droplet tags; the cluster-specific bootstrap and the drain are
// delivered over the on-host agent's mutually authenticated TLS channel (pinned
// server CA + per-machine bearer token, not mTLS; DigitalOcean exposes no
// in-guest command API), so the base image must ship the agent that the generic
// Create-time user_data configures.
type doReal struct {
	cfg    doRealConfig
	client *godo.Client
	logger *slog.Logger

	// sizesMu guards a short-lived cache of the Sizes.List catalogue, so a
	// price/size refresh over N offered sizes is a single catalogue scan.
	sizesMu    sync.Mutex
	sizesCache []godo.Size
	sizesAt    time.Time
}

func newDOReal(cfg doRealConfig, logger *slog.Logger) (*doReal, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("digitalocean: token is required for the digitalocean backend")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("digitalocean: --region is required for the digitalocean backend")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("digitalocean: --image is required for the digitalocean backend")
	}
	if cfg.Vault == nil {
		return nil, fmt.Errorf("digitalocean: the bootstrap agent channel is required (configure --bootstrap-addr + --bootstrap-tls-cert/key)")
	}
	if cfg.BootstrapEndpoint == "" {
		return nil, fmt.Errorf("digitalocean: --bootstrap-endpoint is required so the on-host agent can reach the provider")
	}
	cfg.withDefaults()
	return &doReal{
		cfg:    cfg,
		client: godo.NewFromToken(cfg.Token),
		logger: logger,
	}, nil
}

func (r *doReal) CreateDroplet(ctx context.Context, spec dropletSpec) (dropletInstance, error) {
	// This process owns exactly one region; refuse to create a Droplet anywhere
	// else (a mis-scoped offering would otherwise place hosts outside the region
	// this process manages and reconciles).
	if spec.Region != "" && spec.Region != r.cfg.Region {
		return dropletInstance{}, fmt.Errorf("create droplet: offering region %q does not match this provider's region %q", spec.Region, r.cfg.Region)
	}
	// Bake the generic, pre-binding agent bootstrap into user_data: the operator's
	// base user-data (installs/starts the agent) plus the agent config carrying
	// this Droplet's per-machine token and the provider's pinned endpoint/CA. The
	// cluster-specific blob is delivered later over the agent channel, because
	// user_data is read-only after first boot.
	token := r.cfg.Vault.Token(spec.MachineID)
	agentCfg := agentCloudConfig(r.cfg.BootstrapEndpoint, r.cfg.BootstrapCAPEM, spec.MachineID, token)
	userData := combineUserData(spec.BaseUserData, agentCfg)

	createReq := &godo.DropletCreateRequest{
		Name:     dropletName(spec),
		Region:   r.cfg.Region,
		Size:     spec.Size,
		Image:    dropletImage(spec.Image),
		UserData: userData,
		Tags: []string{
			tagManaged,
			tagMachinePrefix + encodeID(spec.MachineID),
		},
	}
	// Pre-create idempotency. godo exposes no client-side idempotency token and
	// DigitalOcean does NOT enforce unique Droplet names, so a retried or
	// re-dispatched Create with the same OperationID (a previous attempt whose
	// waitActive timed out, or a provider restart mid-create) would otherwise
	// launch a SECOND, untracked, billed Droplet. Look first for the Droplet this
	// operation already created (same derived name + this machine's id tag) and
	// reuse it, rather than relying only on post-error recovery.
	if existing, err := r.existingManagedDroplet(ctx, createReq.Name, spec.MachineID); err != nil {
		return dropletInstance{}, fmt.Errorf("create droplet: pre-create lookup: %w", err)
	} else if existing != nil {
		return r.waitActive(ctx, existing.ID)
	}
	drv, _, err := r.client.Droplets.Create(ctx, createReq)
	if err != nil {
		// Post-error recovery: a create that errored but actually landed (or raced
		// a concurrent attempt). Reuse the existing managed Droplet, verifying the
		// machine-id tag so an unrelated same-named Droplet is never adopted.
		if existing, lerr := r.existingManagedDroplet(ctx, createReq.Name, spec.MachineID); lerr == nil && existing != nil {
			return r.waitActive(ctx, existing.ID)
		}
		return dropletInstance{}, fmt.Errorf("create droplet %s: %w", spec.Size, err)
	}
	if drv == nil || drv.ID == 0 {
		return dropletInstance{}, fmt.Errorf("create droplet %s: empty result", spec.Size)
	}
	return r.waitActive(ctx, drv.ID)
}

// waitActive polls until the Droplet reaches the active status (so the kit's IDLE
// means "reachable host" and the immediately-following Configure does not race a
// still-provisioning Droplet). ctx (the kit's Create timeout) cancels it.
func (r *doReal) waitActive(ctx context.Context, id int) (dropletInstance, error) {
	deadline := time.Now().Add(r.cfg.CreateWaitTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		drv, _, err := r.client.Droplets.Get(ctx, id)
		switch {
		case err != nil:
			// Transient (rate-limit 429, 5xx, or a read-after-write 404 right after
			// create) — keep polling until the deadline instead of failing the whole
			// Create on a single blip. ctx cancellation still aborts below.
			lastErr = err
		case drv == nil:
			lastErr = fmt.Errorf("droplet %d not visible yet", id)
		case drv.Status == "active":
			return r.toDropletInstance(drv), nil
		default:
			lastErr = fmt.Errorf("droplet %d status %q", id, drv.Status)
		}
		select {
		case <-ctx.Done():
			return dropletInstance{}, fmt.Errorf("waiting for droplet %d to become active: %w (last: %v)", id, ctx.Err(), lastErr)
		case <-ticker.C:
			if time.Now().After(deadline) {
				return dropletInstance{}, fmt.Errorf("droplet %d did not become active within %s (last: %v)", id, r.cfg.CreateWaitTimeout, lastErr)
			}
		}
	}
}

func (r *doReal) DeleteDroplet(ctx context.Context, dropletID string) error {
	id, err := strconv.Atoi(dropletID)
	if err != nil {
		return fmt.Errorf("delete: bad droplet id %q: %w", dropletID, err)
	}
	resp, err := r.client.Droplets.Delete(ctx, id)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("delete droplet %d: %w", id, err)
	}
	return nil
}

func (r *doReal) DescribeManaged(ctx context.Context) ([]dropletInstance, error) {
	var out []dropletInstance
	opt := &godo.ListOptions{Page: 1, PerPage: 200}
	for {
		droplets, resp, err := r.client.Droplets.ListByTag(ctx, tagManaged, opt)
		if err != nil {
			return nil, fmt.Errorf("list managed droplets: %w", err)
		}
		for i := range droplets {
			drv := r.toDropletInstance(&droplets[i])
			// DigitalOcean tags are account-wide, so the bigfleet-managed tag also
			// matches this account's Droplets in OTHER regions (managed by sibling
			// provider processes). This process owns exactly one region — drop the
			// rest so they never leak into its inventory or host resolution.
			if drv.Region != r.cfg.Region {
				continue
			}
			out = append(out, drv)
		}
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			// Never return a partial inventory silently: an incomplete list would
			// look like missing Droplets and could re-seed capacity incorrectly.
			return nil, fmt.Errorf("list managed droplets: bad pagination link: %w", err)
		}
		opt.Page = page + 1
	}
	return out, nil
}

func (r *doReal) ApplyBootstrap(ctx context.Context, drv dropletInstance, clusterID string, bootstrap []byte) error {
	if drv.MachineID == "" {
		return fmt.Errorf("configure: droplet %s carries no machine id tag", drv.DropletID)
	}
	// Deliver the opaque blob to the running Droplet over the agent channel and
	// wait for the agent to apply it — a failed join surfaces as FAILED.
	cmd := bootstrapCommand{
		Type:      "configure",
		ClusterID: clusterID,
		Blob:      base64.StdEncoding.EncodeToString(bootstrap),
	}
	if err := r.cfg.Vault.Enqueue(ctx, drv.MachineID, cmd); err != nil {
		return err
	}
	// Record the binding tag only AFTER the bootstrap actually succeeded, so a
	// failed Configure never leaves a Droplet tagged as bound to a cluster it
	// never joined.
	return r.setClusterTag(ctx, drv.DropletID, clusterID)
}

func (r *doReal) DrainNode(ctx context.Context, drv dropletInstance, gracePeriodSeconds int64) error {
	if drv.MachineID == "" {
		return fmt.Errorf("drain: droplet %s carries no machine id tag", drv.DropletID)
	}
	cmd := bootstrapCommand{Type: "drain", GraceSeconds: drainGrace(gracePeriodSeconds)}
	if err := r.cfg.Vault.Enqueue(ctx, drv.MachineID, cmd); err != nil {
		return err
	}
	return r.clearClusterTag(ctx, drv.DropletID, drv.ClusterID)
}

func (r *doReal) PriceUSD(ctx context.Context, sizeSlug string) (float64, error) {
	sizes, err := r.listAllSizes(ctx)
	if err != nil {
		return 0, err
	}
	for _, s := range sizes {
		if s.Slug == sizeSlug {
			return s.PriceHourly, nil
		}
	}
	return 0, fmt.Errorf("no pricing for size %q", sizeSlug)
}

func (r *doReal) DescribeSizeCapacities(ctx context.Context, sizeSlugs []string) (map[string]sizeCapacity, error) {
	want := make(map[string]struct{}, len(sizeSlugs))
	for _, s := range sizeSlugs {
		want[s] = struct{}{}
	}
	sizes, err := r.listAllSizes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]sizeCapacity, len(sizeSlugs))
	for _, s := range sizes {
		if _, ok := want[s.Slug]; ok {
			out[s.Slug] = sizeCapacity{VCPU: s.Vcpus, MemMiB: int64(s.Memory)}
		}
	}
	return out, nil
}

// listAllSizes returns the full DigitalOcean Sizes catalogue, served from a
// short-lived cache so a refresh over many offered sizes is a single paginated
// scan rather than one per size (PriceUSD/DescribeSizeCapacities both read it). A
// pagination error fails the whole fetch — callers must never act on a partial
// catalogue (a missing size would wrongly fall back to the pinned table).
func (r *doReal) listAllSizes(ctx context.Context) ([]godo.Size, error) {
	r.sizesMu.Lock()
	defer r.sizesMu.Unlock()
	if r.sizesCache != nil && time.Since(r.sizesAt) < r.cfg.SizesCacheTTL {
		return r.sizesCache, nil
	}
	var all []godo.Size
	opt := &godo.ListOptions{Page: 1, PerPage: 200}
	for {
		sizes, resp, err := r.client.Sizes.List(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("list sizes: %w", err)
		}
		all = append(all, sizes...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, fmt.Errorf("list sizes: bad pagination link: %w", err)
		}
		opt.Page = page + 1
	}
	r.sizesCache = all
	r.sizesAt = time.Now()
	return all, nil
}

// --- helpers --------------------------------------------------------------

func (r *doReal) toDropletInstance(drv *godo.Droplet) dropletInstance {
	out := dropletInstance{
		DropletID: strconv.Itoa(drv.ID),
		Size:      drv.SizeSlug,
		Active:    drv.Status == "active" || drv.Status == "new",
	}
	if drv.Region != nil {
		out.Region = drv.Region.Slug
	}
	for _, tag := range drv.Tags {
		switch {
		case strings.HasPrefix(tag, tagMachinePrefix):
			out.MachineID = decodeID(strings.TrimPrefix(tag, tagMachinePrefix))
		case strings.HasPrefix(tag, tagClusterPrefix):
			out.ClusterID = decodeID(strings.TrimPrefix(tag, tagClusterPrefix))
		}
	}
	if ip, err := drv.PublicIPv4(); err == nil {
		out.PublicIPv4 = ip
	}
	return out
}

// existingManagedDroplet finds the Droplet a given Create operation already
// produced: a bigfleet-managed Droplet in THIS region whose name matches the
// derived name and whose machine-id tag matches machineID. Matching on the tag
// (not just the name) means an unrelated same-named Droplet is never adopted.
func (r *doReal) existingManagedDroplet(ctx context.Context, name, machineID string) (*godo.Droplet, error) {
	droplets, _, err := r.client.Droplets.ListByName(ctx, name, &godo.ListOptions{PerPage: 200})
	if err != nil {
		return nil, err
	}
	wantTag := tagMachinePrefix + encodeID(machineID)
	for i := range droplets {
		d := &droplets[i]
		if !hasTag(d.Tags, tagManaged) || !hasTag(d.Tags, wantTag) {
			continue
		}
		if d.Region != nil && d.Region.Slug != r.cfg.Region {
			continue
		}
		return d, nil
	}
	return nil, nil
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// setClusterTag tags the Droplet with its cluster binding (creating the tag if
// needed). DigitalOcean tags are flat strings, so the binding is one
// bfcluster:<base32(cluster)> tag.
func (r *doReal) setClusterTag(ctx context.Context, dropletID, clusterID string) error {
	tag := tagClusterPrefix + encodeID(clusterID)
	if _, _, err := r.client.Tags.Create(ctx, &godo.TagCreateRequest{Name: tag}); err != nil {
		// Create is idempotent in practice; a duplicate is fine. Surface only a
		// hard failure by attempting the tag-resources step regardless.
		r.logger.Debug("create cluster tag", "tag", tag, "err", err)
	}
	id, err := strconv.Atoi(dropletID)
	if err != nil {
		return fmt.Errorf("bad droplet id %q: %w", dropletID, err)
	}
	_, err = r.client.Tags.TagResources(ctx, tag, &godo.TagResourcesRequest{
		Resources: []godo.Resource{{ID: strconv.Itoa(id), Type: godo.DropletResourceType}},
	})
	if err != nil {
		return fmt.Errorf("tag cluster binding: %w", err)
	}
	return nil
}

// clearClusterTag removes the cluster binding tag from the Droplet, returning it
// to an unbound (Idle) state in inventory.
func (r *doReal) clearClusterTag(ctx context.Context, dropletID, clusterID string) error {
	if clusterID == "" {
		return nil
	}
	tag := tagClusterPrefix + encodeID(clusterID)
	_, err := r.client.Tags.UntagResources(ctx, tag, &godo.UntagResourcesRequest{
		Resources: []godo.Resource{{ID: dropletID, Type: godo.DropletResourceType}},
	})
	if err != nil {
		return fmt.Errorf("untag cluster binding: %w", err)
	}
	return nil
}

// dropletName derives a stable Droplet name from the operation id (stable across
// a retried Create), so a transport retry recreates under the same name and the
// create is idempotent.
func dropletName(spec dropletSpec) string {
	token := spec.IdempotencyToken
	if token == "" {
		token = spec.MachineID
	}
	// Hash the token so the name is fixed-length and collision-resistant rather
	// than a truncation of the raw id (which would weaken this derived
	// idempotency key for long ids). base32(sha256)=52 chars + prefix < 63.
	sum := sha256.Sum256([]byte(token))
	return "bigfleet-" + strings.ToLower(idEncoding.EncodeToString(sum[:]))
}

// dropletImage interprets the --image flag as a numeric image id when it is all
// digits, else as an image slug.
func dropletImage(image string) godo.DropletCreateImage {
	if id, err := strconv.Atoi(image); err == nil {
		return godo.DropletCreateImage{ID: id}
	}
	return godo.DropletCreateImage{Slug: image}
}

// combineUserData assembles the cloud-init user-data delivered at Droplet create:
// the operator's base user-data (if any) plus the agent cloud-config. With no
// base it returns the bare agent config; with a base it wraps both in a MIME
// multipart archive cloud-init understands, so the agent injection composes with
// whatever the operator supplied.
func combineUserData(base []byte, agentCfg string) string {
	if len(bytes.TrimSpace(base)) == 0 {
		return agentCfg
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q\nMIME-Version: 1.0\n\n", mw.Boundary())
	addPart := func(ctype string, body []byte) {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ctype)
		h.Set("MIME-Version", "1.0")
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write(body)
	}
	addPart(baseUserDataContentType(base), base)
	addPart("text/cloud-config", []byte(agentCfg))
	_ = mw.Close()
	return header + buf.String()
}

func baseUserDataContentType(base []byte) string {
	s := strings.TrimLeft(string(base), " \t\r\n")
	switch {
	case strings.HasPrefix(s, "#cloud-config"):
		return "text/cloud-config"
	case strings.HasPrefix(s, "#!"):
		return "text/x-shellscript"
	case strings.HasPrefix(s, "#cloud-boothook"):
		return "text/cloud-boothook"
	default:
		return "text/cloud-config"
	}
}

var _ doClient = (*doReal)(nil)
