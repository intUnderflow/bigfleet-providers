package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/computeinstanceagent"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// BigFleet freeform-tag keys. tagManaged marks our instances so DescribeManaged
// never touches anything else; the rest let inventory and bindings be recovered
// from OCI alone after a restart.
const (
	tagManaged   = "bigfleet-managed"
	tagMachineID = "bigfleet-machine-id"
	tagCluster   = "bigfleet-cluster"
)

// ociRealConfig is the launch configuration for the production OCI Compute client.
type ociRealConfig struct {
	Region          string
	CompartmentOCID string
	SubnetOCID      string // CreateVnicDetails subnet for LaunchInstance
	ImageOCID       string // base image for LaunchInstance
	AuthMode        string // instance_principal | workload_identity | config_file | auto

	// BootstrapHookPath is the executable on the base image that consumes the
	// delivered bootstrap blob (written to <path>.blob) and joins the cluster.
	BootstrapHookPath string

	// CreateWaitTimeout caps how long LaunchInstance waits for RUNNING (the kit's
	// Create timeout, carried on ctx, usually fires first).
	CreateWaitTimeout time.Duration
	// CommandTimeout caps how long ApplyBootstrap/DrainNode wait for the Run
	// Command execution to finish.
	CommandTimeout time.Duration
	// PollInterval is how often the client polls instance / command status.
	PollInterval time.Duration
}

func (c *ociRealConfig) withDefaults() {
	if c.BootstrapHookPath == "" {
		c.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 10 * time.Minute
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = 8 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
}

// ociReal is the production ociClient, backed by oci-go-sdk. Inventory and
// bindings are recovered from instance freeform tags; the cluster-specific
// bootstrap and the drain are delivered over the Oracle Cloud Agent Run Command
// (OCI IAM-authenticated), so the base image must run the Oracle Cloud Agent with
// the Run Command plugin enabled.
type ociReal struct {
	cfg    ociRealConfig
	comp   core.ComputeClient
	agent  computeinstanceagent.ComputeInstanceAgentClient
	logger *slog.Logger
}

func newOCIReal(_ context.Context, cfg ociRealConfig, logger *slog.Logger) (*ociReal, error) {
	if cfg.CompartmentOCID == "" {
		return nil, fmt.Errorf("oci: --compartment is required for the oci backend")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("oci: --region is required for the oci backend")
	}
	if cfg.SubnetOCID == "" {
		return nil, fmt.Errorf("oci: --subnet is required for the oci backend")
	}
	if cfg.ImageOCID == "" {
		return nil, fmt.Errorf("oci: --image is required for the oci backend")
	}
	cfg.withDefaults()

	provider, err := configurationProvider(cfg.AuthMode, logger)
	if err != nil {
		return nil, err
	}
	comp, err := core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("oci: build compute client: %w", err)
	}
	agent, err := computeinstanceagent.NewComputeInstanceAgentClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("oci: build instance-agent client: %w", err)
	}
	comp.SetRegion(cfg.Region)
	agent.SetRegion(cfg.Region)
	return &ociReal{cfg: cfg, comp: comp, agent: agent, logger: logger}, nil
}

// configurationProvider resolves the OCI auth mode to a common.ConfigurationProvider.
// auto prefers Instance Principals (production on an OCI instance / OKE node) and
// falls back to the config file (~/.oci/config) for local dev.
func configurationProvider(mode string, logger *slog.Logger) (common.ConfigurationProvider, error) {
	switch strings.ToLower(mode) {
	case "instance_principal", "instance-principal", "instanceprincipal":
		return auth.InstancePrincipalConfigurationProvider()
	case "workload_identity", "workload-identity", "oke":
		return auth.OkeWorkloadIdentityConfigurationProvider()
	case "config_file", "config-file", "file":
		return common.DefaultConfigProvider(), nil
	case "auto", "":
		if p, err := auth.InstancePrincipalConfigurationProvider(); err == nil {
			if logger != nil {
				logger.Info("oci auth: using instance principals")
			}
			return p, nil
		} else if logger != nil {
			logger.Info("oci auth: instance principals unavailable, falling back to config file", "err", err)
		}
		return common.DefaultConfigProvider(), nil
	default:
		return nil, fmt.Errorf("oci: unknown --auth mode %q (instance_principal | workload_identity | config_file | auto)", mode)
	}
}

func (r *ociReal) LaunchInstance(ctx context.Context, spec launchSpec) (ociInstance, error) {
	details := core.LaunchInstanceDetails{
		AvailabilityDomain: common.String(spec.AvailabilityDomain),
		CompartmentId:      common.String(r.cfg.CompartmentOCID),
		Shape:              common.String(spec.Shape),
		DisplayName:        common.String(displayName(spec)),
		SourceDetails:      core.InstanceSourceViaImageDetails{ImageId: common.String(r.cfg.ImageOCID)},
		CreateVnicDetails:  &core.CreateVnicDetails{SubnetId: common.String(r.cfg.SubnetOCID)},
		FreeformTags: map[string]string{
			tagManaged:   "true",
			tagMachineID: spec.MachineID,
		},
	}
	if isFlexShape(spec.Shape) {
		details.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
			Ocpus:       common.Float32(float32(spec.OCPUs)),
			MemoryInGBs: common.Float32(float32(spec.MemoryGB)),
		}
	}
	if spec.Preemptible {
		details.PreemptibleInstanceConfig = &core.PreemptibleInstanceConfigDetails{
			PreemptionAction: core.TerminatePreemptionAction{PreserveBootVolume: common.Bool(false)},
		}
	}
	if len(spec.BaseUserData) > 0 {
		details.Metadata = map[string]string{
			"user_data": base64.StdEncoding.EncodeToString(spec.BaseUserData),
		}
	}
	req := core.LaunchInstanceRequest{LaunchInstanceDetails: details}
	// The operation id is the idempotency token: a retried launch maps to the
	// same instance rather than double-provisioning.
	if spec.IdempotencyToken != "" {
		req.OpcRetryToken = common.String(retryToken(spec.IdempotencyToken))
	}
	resp, err := r.comp.LaunchInstance(ctx, req)
	if err != nil {
		return ociInstance{}, fmt.Errorf("LaunchInstance %s: %w", spec.Shape, err)
	}
	if resp.Instance.Id == nil {
		return ociInstance{}, fmt.Errorf("LaunchInstance %s: empty instance id", spec.Shape)
	}
	return r.waitRunning(ctx, *resp.Instance.Id)
}

// waitRunning polls until the instance reaches RUNNING (so the kit's IDLE means
// "reachable host" and the immediately-following Configure does not race a
// still-provisioning instance). ctx (the kit's Create timeout) cancels it.
func (r *ociReal) waitRunning(ctx context.Context, id string) (ociInstance, error) {
	deadline := time.Now().Add(r.cfg.CreateWaitTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		resp, err := r.comp.GetInstance(ctx, core.GetInstanceRequest{InstanceId: common.String(id)})
		if err != nil {
			return ociInstance{}, fmt.Errorf("poll instance %s: %w", id, err)
		}
		switch resp.Instance.LifecycleState {
		case core.InstanceLifecycleStateRunning:
			return r.toInstance(resp.Instance), nil
		case core.InstanceLifecycleStateTerminating, core.InstanceLifecycleStateTerminated, core.InstanceLifecycleStateStopped:
			return ociInstance{}, fmt.Errorf("instance %s entered %s while creating", id, resp.Instance.LifecycleState)
		}
		select {
		case <-ctx.Done():
			return ociInstance{}, fmt.Errorf("waiting for instance %s to run: %w", id, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return ociInstance{}, fmt.Errorf("instance %s did not reach RUNNING within %s", id, r.cfg.CreateWaitTimeout)
			}
		}
	}
}

func (r *ociReal) TerminateInstance(ctx context.Context, instanceID string) error {
	_, err := r.comp.TerminateInstance(ctx, core.TerminateInstanceRequest{
		InstanceId:         common.String(instanceID),
		PreserveBootVolume: common.Bool(false),
	})
	if err != nil {
		if isNotFound(err) {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("TerminateInstance %s: %w", instanceID, err)
	}
	return nil
}

func (r *ociReal) DescribeManaged(ctx context.Context) ([]ociInstance, error) {
	var out []ociInstance
	var page *string
	for {
		resp, err := r.comp.ListInstances(ctx, core.ListInstancesRequest{
			CompartmentId: common.String(r.cfg.CompartmentOCID),
			Page:          page,
		})
		if err != nil {
			return nil, fmt.Errorf("ListInstances: %w", err)
		}
		for _, inst := range resp.Items {
			if inst.FreeformTags[tagManaged] != "true" {
				continue
			}
			out = append(out, r.toInstance(inst))
		}
		if resp.OpcNextPage == nil {
			break
		}
		page = resp.OpcNextPage
	}
	return out, nil
}

func (r *ociReal) ApplyBootstrap(ctx context.Context, inst ociInstance, clusterID string, bootstrap []byte) error {
	// Deliver the opaque bootstrap blob to the node and run the base image's hook.
	// The image must ship the hook at BootstrapHookPath; it receives the blob at
	// <hook>.blob and joins the cluster. We wait for the command to SUCCEED, so a
	// failed bootstrap surfaces as FAILED.
	blob := base64.StdEncoding.EncodeToString(bootstrap) // base64 -d is universally available
	hook := shellQuote(r.cfg.BootstrapHookPath)
	blobPath := shellQuote(r.cfg.BootstrapHookPath + ".blob")
	script := fmt.Sprintf(
		"set -euo pipefail; umask 077; mkdir -p \"$(dirname %s)\"; echo %s | base64 -d > %s; %s %s",
		blobPath, shellQuote(blob), blobPath, hook, shellQuote(clusterID))
	if err := r.runCommand(ctx, inst.InstanceID, "bigfleet-configure", script); err != nil {
		return err
	}
	// Record the binding tag only AFTER the bootstrap actually succeeded, so a
	// failed Configure never leaves an instance mistagged as bound to a cluster it
	// never joined.
	if err := r.setTag(ctx, inst.InstanceID, tagCluster, clusterID); err != nil {
		return fmt.Errorf("tag cluster binding: %w", err)
	}
	return nil
}

func (r *ociReal) DrainNode(ctx context.Context, inst ociInstance, gracePeriodSeconds int64) error {
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
	if err := r.runCommand(ctx, inst.InstanceID, "bigfleet-drain", script); err != nil {
		return err
	}
	return r.clearTag(ctx, inst.InstanceID, tagCluster)
}

// runCommand issues a Run Command against the instance's Oracle Cloud Agent and
// waits for it to finish, returning an error unless it SUCCEEDED. The channel is
// authenticated by OCI IAM (the provider's principal), the control-plane analogue
// of AWS SSM SendCommand.
func (r *ociReal) runCommand(ctx context.Context, instanceID, name, script string) error {
	timeout := int(r.cfg.CommandTimeout.Seconds())
	if timeout <= 0 {
		timeout = 480
	}
	resp, err := r.agent.CreateInstanceAgentCommand(ctx, computeinstanceagent.CreateInstanceAgentCommandRequest{
		CreateInstanceAgentCommandDetails: computeinstanceagent.CreateInstanceAgentCommandDetails{
			CompartmentId:             common.String(r.cfg.CompartmentOCID),
			ExecutionTimeOutInSeconds: common.Int(timeout),
			DisplayName:               common.String(name),
			Target:                    &computeinstanceagent.InstanceAgentCommandTarget{InstanceId: common.String(instanceID)},
			Content: &computeinstanceagent.InstanceAgentCommandContent{
				Source: computeinstanceagent.InstanceAgentCommandSourceViaTextDetails{Text: common.String(script)},
				Output: computeinstanceagent.InstanceAgentCommandOutputViaTextDetails{},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("run command %s on %s: %w", name, instanceID, err)
	}
	if resp.InstanceAgentCommand.Id == nil {
		return fmt.Errorf("run command %s on %s: empty command id", name, instanceID)
	}
	return r.waitCommand(ctx, *resp.InstanceAgentCommand.Id, instanceID, name)
}

func (r *ociReal) waitCommand(ctx context.Context, commandID, instanceID, name string) error {
	deadline := time.Now().Add(r.cfg.CommandTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		resp, err := r.agent.GetInstanceAgentCommandExecution(ctx, computeinstanceagent.GetInstanceAgentCommandExecutionRequest{
			InstanceAgentCommandId: common.String(commandID),
			InstanceId:             common.String(instanceID),
		})
		if err != nil {
			return fmt.Errorf("poll command %s on %s: %w", name, instanceID, err)
		}
		switch resp.InstanceAgentCommandExecution.LifecycleState {
		case computeinstanceagent.InstanceAgentCommandExecutionLifecycleStateSucceeded:
			return nil
		case computeinstanceagent.InstanceAgentCommandExecutionLifecycleStateFailed:
			return fmt.Errorf("command %s on %s failed", name, instanceID)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for command %s on %s: %w", name, instanceID, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("command %s on %s did not finish within %s", name, instanceID, r.cfg.CommandTimeout)
			}
		}
	}
}

// --- helpers --------------------------------------------------------------

func (r *ociReal) toInstance(inst core.Instance) ociInstance {
	out := ociInstance{
		MachineID:   inst.FreeformTags[tagMachineID],
		ClusterID:   inst.FreeformTags[tagCluster],
		Preemptible: inst.PreemptibleInstanceConfig != nil,
		Running: inst.LifecycleState == core.InstanceLifecycleStateRunning ||
			inst.LifecycleState == core.InstanceLifecycleStateProvisioning ||
			inst.LifecycleState == core.InstanceLifecycleStateStarting,
	}
	if inst.Id != nil {
		out.InstanceID = *inst.Id
	}
	if inst.Shape != nil {
		out.Shape = *inst.Shape
	}
	if inst.AvailabilityDomain != nil {
		out.AvailabilityDomain = *inst.AvailabilityDomain
	}
	if inst.ShapeConfig != nil {
		if inst.ShapeConfig.Ocpus != nil {
			out.OCPUs = float64(*inst.ShapeConfig.Ocpus)
		}
		if inst.ShapeConfig.MemoryInGBs != nil {
			out.MemoryGB = float64(*inst.ShapeConfig.MemoryInGBs)
		}
	}
	return out
}

func (r *ociReal) setTag(ctx context.Context, instanceID, key, value string) error {
	return r.updateTags(ctx, instanceID, func(tags map[string]string) { tags[key] = value })
}

func (r *ociReal) clearTag(ctx context.Context, instanceID, key string) error {
	return r.updateTags(ctx, instanceID, func(tags map[string]string) { delete(tags, key) })
}

func (r *ociReal) updateTags(ctx context.Context, instanceID string, mutate func(map[string]string)) error {
	resp, err := r.comp.GetInstance(ctx, core.GetInstanceRequest{InstanceId: common.String(instanceID)})
	if err != nil {
		return fmt.Errorf("get instance %s: %w", instanceID, err)
	}
	tags := map[string]string{}
	for k, v := range resp.Instance.FreeformTags {
		tags[k] = v
	}
	mutate(tags)
	_, err = r.comp.UpdateInstance(ctx, core.UpdateInstanceRequest{
		InstanceId:            common.String(instanceID),
		UpdateInstanceDetails: core.UpdateInstanceDetails{FreeformTags: tags},
	})
	return err
}

// isNotFound reports whether err is an OCI 404 (so an idempotent terminate of an
// already-gone instance is not treated as a failure).
func isNotFound(err error) bool {
	if se, ok := common.IsServiceError(err); ok {
		return se.GetHTTPStatusCode() == 404
	}
	return false
}

// displayName / retryToken derive stable, OCI-safe identifiers from the operation
// id (stable across a retried launch), so a transport retry maps to the same
// instance and the create is idempotent.
func displayName(spec launchSpec) string {
	token := spec.IdempotencyToken
	if token == "" {
		token = spec.MachineID
	}
	name := "bigfleet-" + sanitize(token)
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

func retryToken(token string) string {
	if len(token) > 64 {
		return token[:64]
	}
	return token
}

// sanitize maps a machine/operation id to a display-name-safe slug.
func sanitize(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// shellQuote single-quotes a string for safe interpolation into a /bin/sh command
// (the blob and cluster id are opaque, so never trust their bytes).
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

var _ ociClient = (*ociReal)(nil)
