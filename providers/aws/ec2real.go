package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// BigFleet tag keys. bigfleet:managed marks our instances so DescribeManaged
// never touches anything else; the rest let inventory and bindings be
// recovered from EC2 alone.
const (
	tagManaged   = "bigfleet:managed"
	tagMachineID = "bigfleet:machine-id"
	tagCluster   = "bigfleet:cluster"
	tagCapacity  = "bigfleet:capacity"
)

// ec2API and ssmAPI are the exact AWS SDK methods the real client uses, so the
// production client can be unit-tested by injecting fakes. *ec2.Client and
// *ssm.Client satisfy them. The interfaces double as the argument to the SDK's
// DescribeInstances paginator and instance-running waiter (both want only
// DescribeInstances).
type ec2API interface {
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	CreateTags(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(context.Context, *ec2.DeleteTagsInput, ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)
	DescribeSpotPriceHistory(context.Context, *ec2.DescribeSpotPriceHistoryInput, ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
}

type ssmAPI interface {
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

// ec2RealConfig is the launch configuration for the production EC2 client.
type ec2RealConfig struct {
	Region             string
	AMI                string            // base AMI for RunInstances
	Subnets            map[string]string // availability zone -> subnet id
	SecurityGroupIDs   []string
	IAMInstanceProfile string // instance profile name (needs SSM perms for Configure/Drain)
	KeyName            string // optional SSH key
	// BootstrapHookPath is the executable on the AMI that consumes the
	// delivered bootstrap blob (written to <path>.blob) and joins the cluster.
	BootstrapHookPath string
	// MaxRetryAttempts bounds the SDK's automatic retries (throttling, 5xx).
	MaxRetryAttempts int
	// RunWaitTimeout caps how long RunInstance waits for the instance to reach
	// 'running' (the kit's Create timeout, carried on ctx, usually fires first).
	RunWaitTimeout time.Duration
	// SSMPollInterval is how often Configure/Drain poll an SSM command's status.
	SSMPollInterval time.Duration
}

func (c *ec2RealConfig) withDefaults() {
	if c.BootstrapHookPath == "" {
		c.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	if c.MaxRetryAttempts <= 0 {
		c.MaxRetryAttempts = 8
	}
	if c.RunWaitTimeout <= 0 {
		c.RunWaitTimeout = 10 * time.Minute
	}
	if c.SSMPollInterval <= 0 {
		c.SSMPollInterval = 3 * time.Second
	}
}

// ec2Real is the production ec2Client, backed by aws-sdk-go-v2. Inventory and
// bindings are recovered from EC2 tags; the cluster-specific bootstrap and the
// drain are delivered via SSM (the instance profile must grant SSM).
type ec2Real struct {
	cfg    ec2RealConfig
	ec2    ec2API
	ssm    ssmAPI
	logger *slog.Logger
}

func newEC2Real(ctx context.Context, cfg ec2RealConfig, logger *slog.Logger) (*ec2Real, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("ec2: region is required")
	}
	if cfg.AMI == "" {
		return nil, fmt.Errorf("ec2: --ami is required for the aws backend")
	}
	cfg.withDefaults()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		// Adaptive retry adds client-side rate limiting on top of backoff, so a
		// throttled RunInstances/DescribeInstances retries instead of failing
		// the transition. Create's kit timeout must exceed the max backoff.
		awsconfig.WithRetryMaxAttempts(cfg.MaxRetryAttempts),
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
	)
	if err != nil {
		return nil, fmt.Errorf("ec2: load AWS config: %w", err)
	}
	return &ec2Real{
		cfg:    cfg,
		ec2:    ec2.NewFromConfig(awsCfg),
		ssm:    ssm.NewFromConfig(awsCfg),
		logger: logger,
	}, nil
}

func (r *ec2Real) RunInstance(ctx context.Context, spec runSpec) (ec2Instance, error) {
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(r.cfg.AMI),
		InstanceType: ec2types.InstanceType(spec.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags: []ec2types.Tag{
				{Key: aws.String(tagManaged), Value: aws.String("true")},
				{Key: aws.String(tagMachineID), Value: aws.String(spec.MachineID)},
				{Key: aws.String(tagCapacity), Value: aws.String(spec.Capacity)},
			},
		}},
	}
	// EC2-level idempotency: with a ClientToken, a retried RunInstances within
	// AWS's ~24h window returns the SAME instance instead of launching a second
	// one. The token is the kit's per-operation id (fresh for a new Create
	// cycle, stable across retries of the same one), so a Create→Delete→Create
	// still launches fresh while a transport retry never double-provisions.
	if spec.IdempotencyToken != "" {
		input.ClientToken = aws.String(spec.IdempotencyToken)
	}
	if sub, ok := r.cfg.Subnets[spec.Zone]; ok && sub != "" {
		input.SubnetId = aws.String(sub)
	} else if spec.Zone != "" {
		input.Placement = &ec2types.Placement{AvailabilityZone: aws.String(spec.Zone)}
	}
	if len(r.cfg.SecurityGroupIDs) > 0 {
		input.SecurityGroupIds = r.cfg.SecurityGroupIDs
	}
	if r.cfg.IAMInstanceProfile != "" {
		input.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{Name: aws.String(r.cfg.IAMInstanceProfile)}
	}
	if r.cfg.KeyName != "" {
		input.KeyName = aws.String(r.cfg.KeyName)
	}
	if len(spec.BaseUserData) > 0 {
		input.UserData = aws.String(base64.StdEncoding.EncodeToString(spec.BaseUserData))
	}
	if spec.Spot {
		input.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
			SpotOptions: &ec2types.SpotMarketOptions{
				SpotInstanceType:             ec2types.SpotInstanceTypeOneTime,
				InstanceInterruptionBehavior: ec2types.InstanceInterruptionBehaviorTerminate,
			},
		}
	}

	out, err := r.ec2.RunInstances(ctx, input)
	if err != nil {
		return ec2Instance{}, err
	}
	if len(out.Instances) == 0 || aws.ToString(out.Instances[0].InstanceId) == "" {
		return ec2Instance{}, fmt.Errorf("RunInstances %s returned no instance id", spec.InstanceType)
	}
	id := aws.ToString(out.Instances[0].InstanceId)

	// Block until the host is actually running before returning, so the kit's
	// IDLE means "reachable host" and the immediately-following Configure does
	// not race a still-pending instance. ctx (the kit's Create timeout) cancels
	// this if the instance never comes up.
	waiter := ec2.NewInstanceRunningWaiter(r.ec2)
	desc, err := waiter.WaitForOutput(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, r.cfg.RunWaitTimeout)
	if err != nil {
		return ec2Instance{}, fmt.Errorf("waiting for instance %s to run: %w", id, err)
	}
	if inst, ok := firstInstance(desc); ok {
		return r.toEC2Instance(inst), nil
	}
	// Waiter reported running but the describe was empty (eventual consistency);
	// fall back to the launch result.
	return r.toEC2Instance(out.Instances[0]), nil
}

func firstInstance(out *ec2.DescribeInstancesOutput) (ec2types.Instance, bool) {
	if out == nil {
		return ec2types.Instance{}, false
	}
	for _, res := range out.Reservations {
		if len(res.Instances) > 0 {
			return res.Instances[0], true
		}
	}
	return ec2types.Instance{}, false
}

func (r *ec2Real) TerminateInstance(ctx context.Context, instanceID string) error {
	_, err := r.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	return err
}

func (r *ec2Real) DescribeManaged(ctx context.Context) ([]ec2Instance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagManaged), Values: []string{"true"}},
			// Alive states only — a shutting-down/terminated instance is
			// releasing its slot, so it is correctly absent here (the slot then
			// returns to Speculative for re-provisioning).
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	}
	var out []ec2Instance
	paginator := ec2.NewDescribeInstancesPaginator(r.ec2, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				out = append(out, r.toEC2Instance(inst))
			}
		}
	}
	return out, nil
}

func (r *ec2Real) ApplyBootstrap(ctx context.Context, instanceID, clusterID string, bootstrap []byte) error {
	// Record the binding as a tag so it is recoverable from EC2 alone.
	if _, err := r.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags:      []ec2types.Tag{{Key: aws.String(tagCluster), Value: aws.String(clusterID)}},
	}); err != nil {
		return fmt.Errorf("tag cluster binding: %w", err)
	}
	// Deliver the opaque bootstrap blob to the node and run the AMI's hook.
	// The AMI must ship the hook at cfg.BootstrapHookPath; it receives the blob
	// at <hook>.blob and joins the cluster. We wait for the command to SUCCEED
	// (not merely enqueue), so a failed bootstrap surfaces as FAILED.
	blob := base64.StdEncoding.EncodeToString(bootstrap)
	hook := shellQuote(r.cfg.BootstrapHookPath)
	blobPath := shellQuote(r.cfg.BootstrapHookPath + ".blob")
	script := fmt.Sprintf(
		"set -euo pipefail; umask 077; echo %s | base64 -d > %s; %s %s",
		shellQuote(blob), blobPath, hook, shellQuote(clusterID))
	return r.runCommand(ctx, instanceID, "bigfleet-configure", script, 600)
}

func (r *ec2Real) DrainNode(ctx context.Context, instanceID string, gracePeriodSeconds int64) error {
	// Remove the binding tag and cordon+drain the kubelet off this node.
	if _, err := r.ec2.DeleteTags(ctx, &ec2.DeleteTagsInput{
		Resources: []string{instanceID},
		Tags:      []ec2types.Tag{{Key: aws.String(tagCluster)}},
	}); err != nil {
		return fmt.Errorf("remove cluster binding tag: %w", err)
	}
	grace := gracePeriodSeconds
	if grace <= 0 {
		grace = 1
	}
	// On EC2 with the AWS cloud provider the Kubernetes node name is the
	// instance's private DNS name, which `hostname -f` returns; fall back to the
	// short hostname only if the FQDN is unavailable. cordon tolerates a re-run
	// (|| true); the DRAIN must NOT swallow its failure — an incomplete drain
	// has to surface as FAILED rather than a false Idle, so the command exits
	// non-zero and the SSM-completion poll below sees it.
	script := fmt.Sprintf(
		"set -euo pipefail; node=$(hostname -f 2>/dev/null || hostname); "+
			"kubectl cordon \"$node\" || true; "+
			"kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds",
		grace, grace)
	return r.runCommand(ctx, instanceID, "bigfleet-drain", script, grace+60)
}

func (r *ec2Real) SpotPriceUSD(ctx context.Context, instanceType, zone string) (float64, error) {
	start := time.Now().Add(-6 * time.Hour)
	out, err := r.ec2.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:    []ec2types.InstanceType{ec2types.InstanceType(instanceType)},
		AvailabilityZone: aws.String(zone),
		// Both the classic and modern-VPC product descriptions; VPC is the
		// default for current accounts.
		ProductDescriptions: []string{"Linux/UNIX", "Linux/UNIX (Amazon VPC)"},
		StartTime:           aws.Time(start),
		MaxResults:          aws.Int32(20),
	})
	if err != nil {
		return 0, err
	}
	var newest time.Time
	var price float64
	var found bool
	for _, sp := range out.SpotPriceHistory {
		if sp.Timestamp == nil || sp.SpotPrice == nil {
			continue
		}
		if !found || sp.Timestamp.After(newest) {
			v, perr := strconv.ParseFloat(*sp.SpotPrice, 64)
			if perr != nil {
				continue
			}
			newest, price, found = *sp.Timestamp, v, true
		}
	}
	if !found {
		return 0, fmt.Errorf("no spot price history for %s in %s", instanceType, zone)
	}
	return price, nil
}

// runCommand sends an SSM shell command and waits for it to actually complete,
// returning an error unless it ends in Success. This is what makes Configure /
// Drain real: an SSM SendCommand that merely ENQUEUES is not success.
func (r *ec2Real) runCommand(ctx context.Context, instanceID, comment, script string, timeoutSeconds int64) error {
	out, err := r.ssm.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:    []string{instanceID},
		DocumentName:   aws.String("AWS-RunShellScript"),
		Comment:        aws.String(comment),
		TimeoutSeconds: aws.Int32(int32(timeoutSeconds)),
		Parameters:     map[string][]string{"commands": {script}},
	})
	if err != nil {
		return fmt.Errorf("ssm SendCommand (%s): %w", comment, err)
	}
	if out.Command == nil || aws.ToString(out.Command.CommandId) == "" {
		return fmt.Errorf("ssm SendCommand (%s): no command id", comment)
	}
	return r.waitCommand(ctx, aws.ToString(out.Command.CommandId), instanceID, comment)
}

func (r *ec2Real) waitCommand(ctx context.Context, commandID, instanceID, comment string) error {
	ticker := time.NewTicker(r.cfg.SSMPollInterval)
	defer ticker.Stop()
	for {
		inv, err := r.ssm.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			var dne *ssmtypes.InvocationDoesNotExist
			if !errors.As(err, &dne) { // not-yet-registered is transient; keep polling
				return fmt.Errorf("ssm GetCommandInvocation (%s): %w", comment, err)
			}
		} else {
			switch inv.Status {
			case ssmtypes.CommandInvocationStatusSuccess:
				return nil
			case ssmtypes.CommandInvocationStatusFailed,
				ssmtypes.CommandInvocationStatusTimedOut,
				ssmtypes.CommandInvocationStatusCancelled:
				return fmt.Errorf("ssm command %q on %s ended %s: %s",
					comment, instanceID, inv.Status, aws.ToString(inv.StandardErrorContent))
			}
			// Pending / InProgress / Delayed / Cancelling — keep polling.
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ssm command %q on %s did not complete: %w", comment, instanceID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// toEC2Instance maps an SDK instance into the substrate-only view.
func (r *ec2Real) toEC2Instance(inst ec2types.Instance) ec2Instance {
	out := ec2Instance{
		InstanceID:   aws.ToString(inst.InstanceId),
		InstanceType: string(inst.InstanceType),
		PrivateDNS:   aws.ToString(inst.PrivateDnsName),
		Spot:         inst.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot,
	}
	if inst.Placement != nil {
		out.Zone = aws.ToString(inst.Placement.AvailabilityZone)
	}
	if inst.State != nil {
		out.Running = inst.State.Name == ec2types.InstanceStateNamePending ||
			inst.State.Name == ec2types.InstanceStateNameRunning
	}
	for _, tag := range inst.Tags {
		switch aws.ToString(tag.Key) {
		case tagMachineID:
			out.MachineID = aws.ToString(tag.Value)
		case tagCluster:
			out.ClusterID = aws.ToString(tag.Value)
		case tagCapacity:
			out.Capacity = aws.ToString(tag.Value)
		}
	}
	return out
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

var _ ec2Client = (*ec2Real)(nil)
