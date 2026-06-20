package main

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// BigFleet instance-label keys. bigfleet-managed marks our instances so
// DescribeManaged never touches anything else; the rest let inventory and
// bindings be recovered from GCE alone. GCE label VALUES are constrained
// (lowercase letters, digits, '-' and '_', max 63 chars), so the (slash-bearing,
// mixed-case) machine id and cluster id are base32-encoded into their values and
// decoded back on read.
const (
	labelManaged   = "bigfleet-managed"
	labelMachineID = "bigfleet-machine-id"
	labelCluster   = "bigfleet-cluster"
	labelCapacity  = "bigfleet-capacity"
)

// startupScriptKey is the GCE metadata key whose value is run on every boot.
// ConfigureInstance writes the cluster bootstrap blob here; DrainInstance
// removes it so the node will not rejoin on a future boot.
const startupScriptKey = "startup-script"

// labelEncoding encodes an arbitrary string into a GCE-label-safe value
// (lowercase base32 without padding → only [a-z2-7], well within the value
// charset). Round-trips any id up to ~39 bytes within the 63-char label limit;
// longer ids rely on the FileStore for restart recovery (the documented primary
// path).
var labelEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func encodeLabel(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToLower(labelEncoding.EncodeToString([]byte(s)))
}

func decodeLabel(v string) string {
	if v == "" {
		return ""
	}
	b, err := labelEncoding.DecodeString(strings.ToUpper(v))
	if err != nil {
		return ""
	}
	return string(b)
}

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
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 5 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
}

// gceReal is the production gceClient, backed by cloud.google.com/go/compute.
// Inventory and bindings are recovered from instance labels; the
// cluster-specific bootstrap is delivered by writing the startup-script metadata
// and resetting the instance (so the base image runs it on the next boot and the
// node joins the cluster). One process per region; DescribeManaged uses
// AggregatedList and filters to the region's zones.
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
			labelManaged:   "true",
			labelMachineID: encodeLabel(spec.MachineID),
			labelCapacity:  spec.Capacity,
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
	if len(spec.BaseStartupScript) > 0 {
		res.Metadata = &computepb.Metadata{Items: []*computepb.Items{{
			Key:   proto.String(startupScriptKey),
			Value: proto.String(string(spec.BaseStartupScript)),
		}}}
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
		// recover the existing instance instead of failing.
		if isAlreadyExists(err) {
			return r.getInstance(ctx, spec.Zone, name)
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
		inst, err := r.getInstance(ctx, zone, name)
		if err != nil {
			return gceInstance{}, err
		}
		if inst.Running {
			return inst, nil
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

func (r *gceReal) ApplyBootstrap(ctx context.Context, inst gceInstance, clusterID string, bootstrap []byte) error {
	live, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project:  r.cfg.Project,
		Zone:     inst.Zone,
		Instance: inst.Name,
	})
	if err != nil {
		return fmt.Errorf("configure: get instance %s: %w", inst.Name, err)
	}
	// Overwrite the startup-script with the cluster bootstrap blob, preserving any
	// other metadata items, then reset so the script runs and the node joins.
	md := setMetadataItem(live.GetMetadata(), startupScriptKey, string(bootstrap))
	if err := r.setMetadata(ctx, inst.Zone, inst.Name, md); err != nil {
		return fmt.Errorf("configure: set metadata %s: %w", inst.Name, err)
	}
	op, err := r.instances.Reset(ctx, &computepb.ResetInstanceRequest{
		Project:  r.cfg.Project,
		Zone:     inst.Zone,
		Instance: inst.Name,
	})
	if err != nil {
		return fmt.Errorf("configure: reset %s: %w", inst.Name, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("configure: wait for reset %s: %w", inst.Name, err)
	}
	// Record the binding label only AFTER the bootstrap was applied, so a failed
	// Configure never leaves an instance mislabelled as bound to a cluster.
	if err := r.setLabel(ctx, inst.Zone, inst.Name, labelCluster, encodeLabel(clusterID)); err != nil {
		return fmt.Errorf("configure: label cluster binding: %w", err)
	}
	return nil
}

func (r *gceReal) DrainNode(ctx context.Context, inst gceInstance, _ int64) error {
	live, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project:  r.cfg.Project,
		Zone:     inst.Zone,
		Instance: inst.Name,
	})
	if err != nil {
		return fmt.Errorf("drain: get instance %s: %w", inst.Name, err)
	}
	// Strip the cluster bootstrap so the node will not rejoin on a future boot,
	// leaving the instance running but unbound. BigFleet has already cordoned and
	// drained the pods at the k8s layer; this is the machine-side cleanup.
	md := removeMetadataItem(live.GetMetadata(), startupScriptKey)
	if err := r.setMetadata(ctx, inst.Zone, inst.Name, md); err != nil {
		return fmt.Errorf("drain: set metadata %s: %w", inst.Name, err)
	}
	return r.clearLabel(ctx, inst.Zone, inst.Name, labelCluster)
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
	return ni
}

func (r *gceReal) getInstance(ctx context.Context, zone, name string) (gceInstance, error) {
	inst, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project:  r.cfg.Project,
		Zone:     zone,
		Instance: name,
	})
	if err != nil {
		return gceInstance{}, fmt.Errorf("get instance %s: %w", name, err)
	}
	return r.toGCEInstance(inst), nil
}

func (r *gceReal) toGCEInstance(inst *computepb.Instance) gceInstance {
	out := gceInstance{
		Name:        inst.GetName(),
		Zone:        lastPathSegment(inst.GetZone()),
		MachineType: lastPathSegment(inst.GetMachineType()),
		MachineID:   decodeLabel(inst.GetLabels()[labelMachineID]),
		ClusterID:   decodeLabel(inst.GetLabels()[labelCluster]),
		Capacity:    inst.GetLabels()[labelCapacity],
		SelfLink:    inst.GetSelfLink(),
		Running:     isRunningStatus(inst.GetStatus()),
	}
	if sched := inst.GetScheduling(); sched != nil {
		out.Spot = sched.GetProvisioningModel() == "SPOT" || sched.GetPreemptible()
	}
	return out
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

func (r *gceReal) setLabel(ctx context.Context, zone, name, key, value string) error {
	return r.updateLabels(ctx, zone, name, func(labels map[string]string) {
		labels[key] = value
	})
}

func (r *gceReal) clearLabel(ctx context.Context, zone, name, key string) error {
	return r.updateLabels(ctx, zone, name, func(labels map[string]string) {
		delete(labels, key)
	})
}

func (r *gceReal) updateLabels(ctx context.Context, zone, name string, mutate func(map[string]string)) error {
	live, err := r.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project:  r.cfg.Project,
		Zone:     zone,
		Instance: name,
	})
	if err != nil {
		return err
	}
	labels := map[string]string{}
	for k, v := range live.GetLabels() {
		labels[k] = v
	}
	mutate(labels)
	op, err := r.instances.SetLabels(ctx, &computepb.SetLabelsInstanceRequest{
		Project:  r.cfg.Project,
		Zone:     zone,
		Instance: name,
		InstancesSetLabelsRequestResource: &computepb.InstancesSetLabelsRequest{
			LabelFingerprint: proto.String(live.GetLabelFingerprint()),
			Labels:           labels,
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
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
	name := "bf-" + strings.ToLower(labelEncoding.EncodeToString([]byte(token)))
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
