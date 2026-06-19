package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
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
}

// ec2Real is the production ec2Client, backed by aws-sdk-go-v2. Inventory and
// bindings are recovered from EC2 tags; the cluster-specific bootstrap and the
// drain are delivered via SSM (the instance profile must grant SSM).
type ec2Real struct {
	cfg    ec2RealConfig
	ec2    *ec2.Client
	ssm    *ssm.Client
	logger *slog.Logger
}

func newEC2Real(ctx context.Context, cfg ec2RealConfig, logger *slog.Logger) (*ec2Real, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("ec2: region is required")
	}
	if cfg.AMI == "" {
		return nil, fmt.Errorf("ec2: --ami is required for the aws backend")
	}
	if cfg.BootstrapHookPath == "" {
		cfg.BootstrapHookPath = "/opt/bigfleet/bootstrap"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
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
	if len(out.Instances) == 0 {
		return ec2Instance{}, fmt.Errorf("RunInstances returned no instances")
	}
	inst := r.toEC2Instance(out.Instances[0])
	if inst.InstanceID == "" {
		return ec2Instance{}, fmt.Errorf("RunInstances %s returned an instance with no id", spec.InstanceType)
	}
	return inst, nil
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
	// at <hook>.blob and joins the cluster.
	blob := base64.StdEncoding.EncodeToString(bootstrap)
	hook := shellQuote(r.cfg.BootstrapHookPath)
	blobPath := shellQuote(r.cfg.BootstrapHookPath + ".blob")
	script := fmt.Sprintf(
		"set -euo pipefail; umask 077; echo %s | base64 -d > %s; %s %s",
		shellQuote(blob), blobPath, hook, shellQuote(clusterID))
	return r.sendCommand(ctx, instanceID, "bigfleet-configure", script, 600)
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
	// instance's private DNS name, which `hostname -f` returns; fall back to
	// the short hostname only if the FQDN is unavailable.
	script := fmt.Sprintf(
		"set -euo pipefail; node=$(hostname -f 2>/dev/null || hostname); "+
			"kubectl cordon \"$node\" || true; "+
			"kubectl drain \"$node\" --ignore-daemonsets --delete-emptydir-data "+
			"--grace-period=%d --timeout=%ds || true",
		grace, grace)
	return r.sendCommand(ctx, instanceID, "bigfleet-drain", script, grace+60)
}

func (r *ec2Real) SpotPriceUSD(ctx context.Context, instanceType, zone string) (float64, error) {
	start := time.Now().Add(-6 * time.Hour)
	out, err := r.ec2.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []ec2types.InstanceType{ec2types.InstanceType(instanceType)},
		AvailabilityZone:    aws.String(zone),
		ProductDescriptions: []string{"Linux/UNIX"},
		StartTime:           aws.Time(start),
		MaxResults:          aws.Int32(10),
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

func (r *ec2Real) sendCommand(ctx context.Context, instanceID, comment, script string, timeoutSeconds int64) error {
	_, err := r.ssm.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:    []string{instanceID},
		DocumentName:   aws.String("AWS-RunShellScript"),
		Comment:        aws.String(comment),
		TimeoutSeconds: aws.Int32(int32(timeoutSeconds)),
		Parameters:     map[string][]string{"commands": {script}},
	})
	if err != nil {
		return fmt.Errorf("ssm SendCommand (%s): %w", comment, err)
	}
	return nil
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
