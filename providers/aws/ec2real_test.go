package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fakeEC2 is an injected ec2API for unit-testing ec2Real without AWS.
type fakeEC2 struct {
	mu sync.Mutex

	runInput  *ec2.RunInstancesInput
	runOut    *ec2.RunInstancesOutput
	runErr    error
	describe  *ec2.DescribeInstancesOutput
	descErr   error
	descInput []*ec2.DescribeInstancesInput
	spot      *ec2.DescribeSpotPriceHistoryOutput
	spotErr   error

	terminated []string
	createTags []*ec2.CreateTagsInput
	deleteTags []*ec2.DeleteTagsInput
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	f.runInput = in
	f.mu.Unlock()
	return f.runOut, f.runErr
}
func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	f.descInput = append(f.descInput, in)
	f.mu.Unlock()
	return f.describe, f.descErr
}
func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	f.terminated = append(f.terminated, in.InstanceIds...)
	f.mu.Unlock()
	return &ec2.TerminateInstancesOutput{}, nil
}
func (f *fakeEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.mu.Lock()
	f.createTags = append(f.createTags, in)
	f.mu.Unlock()
	return &ec2.CreateTagsOutput{}, nil
}
func (f *fakeEC2) DeleteTags(_ context.Context, in *ec2.DeleteTagsInput, _ ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error) {
	f.mu.Lock()
	f.deleteTags = append(f.deleteTags, in)
	f.mu.Unlock()
	return &ec2.DeleteTagsOutput{}, nil
}
func (f *fakeEC2) DescribeSpotPriceHistory(_ context.Context, _ *ec2.DescribeSpotPriceHistoryInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	return f.spot, f.spotErr
}

// fakeSSM is an injected ssmAPI.
type fakeSSM struct {
	mu        sync.Mutex
	sendInput *ssm.SendCommandInput
	status    ssmtypes.CommandInvocationStatus
	stderr    string
	getErr    error
}

func (f *fakeSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.mu.Lock()
	f.sendInput = in
	f.mu.Unlock()
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd-1")}}, nil
}
func (f *fakeSSM) GetCommandInvocation(_ context.Context, _ *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &ssm.GetCommandInvocationOutput{Status: f.status, StandardErrorContent: aws.String(f.stderr)}, nil
}

func (f *fakeSSM) sentScript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendInput == nil {
		return ""
	}
	return strings.Join(f.sendInput.Parameters["commands"], "\n")
}

func newTestReal(e ec2API, s ssmAPI) *ec2Real {
	cfg := ec2RealConfig{Region: "us-east-1", AMI: "ami-123", SSMPollInterval: time.Millisecond, RunWaitTimeout: 5 * time.Second}
	cfg.withDefaults()
	return &ec2Real{cfg: cfg, ec2: e, ssm: s, logger: quietLogger()}
}

func runningInstance(id, az, privateDNS string, tags []ec2types.Tag, spot bool) ec2types.Instance {
	inst := ec2types.Instance{
		InstanceId:     aws.String(id),
		InstanceType:   ec2types.InstanceTypeC7gXlarge,
		PrivateDnsName: aws.String(privateDNS),
		State:          &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Placement:      &ec2types.Placement{AvailabilityZone: aws.String(az)},
		Tags:           tags,
	}
	if spot {
		inst.InstanceLifecycle = ec2types.InstanceLifecycleTypeSpot
	}
	return inst
}

func TestRunInstance_InputClientTokenSpotTagsAndWaiter(t *testing.T) {
	e := &fakeEC2{
		// RunInstances accepts; the instance is still pending here.
		runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{
			InstanceId: aws.String("i-abc"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
		}}},
		// The running-waiter polls DescribeInstances; return it running with the
		// final facts (a different PrivateDNS proves we re-describe).
		describe: &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
			runningInstance("i-abc", "us-east-1a", "ip-10-0-0-9.ec2.internal",
				[]ec2types.Tag{{Key: aws.String(tagMachineID), Value: aws.String("slot-1")}}, true),
		}}}},
	}
	r := newTestReal(e, &fakeSSM{})

	got, err := r.RunInstance(context.Background(), runSpec{
		MachineID: "slot-1", InstanceType: "c7g.xlarge", Zone: "us-east-1a",
		Spot: true, Capacity: "spot", IdempotencyToken: "op-7",
	})
	if err != nil {
		t.Fatalf("RunInstance: %v", err)
	}
	if got.InstanceID != "i-abc" || got.PrivateDNS != "ip-10-0-0-9.ec2.internal" {
		t.Errorf("RunInstance returned %+v; want the running instance from Describe", got)
	}
	if len(e.descInput) == 0 {
		t.Error("running-waiter never called DescribeInstances")
	}

	in := e.runInput
	if aws.ToString(in.ClientToken) != "op-7" {
		t.Errorf("ClientToken = %q, want op-7 (the operation id)", aws.ToString(in.ClientToken))
	}
	if aws.ToInt32(in.MinCount) != 1 || aws.ToInt32(in.MaxCount) != 1 {
		t.Errorf("Min/MaxCount = %d/%d, want 1/1", aws.ToInt32(in.MinCount), aws.ToInt32(in.MaxCount))
	}
	if in.InstanceMarketOptions == nil || in.InstanceMarketOptions.MarketType != ec2types.MarketTypeSpot {
		t.Error("spot machine missing InstanceMarketOptions{MarketType: spot}")
	}
	tags := map[string]string{}
	for _, ts := range in.TagSpecifications {
		for _, tag := range ts.Tags {
			tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
	}
	if tags[tagManaged] != "true" || tags[tagMachineID] != "slot-1" || tags[tagCapacity] != "spot" {
		t.Errorf("launch tags = %v; want managed/machine-id/capacity set", tags)
	}
}

func TestRunInstance_NonSpotNoMarketOptions(t *testing.T) {
	e := &fakeEC2{
		runOut:   &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-1"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending}}}},
		describe: &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{runningInstance("i-1", "us-east-1a", "dns", nil, false)}}}},
	}
	r := newTestReal(e, &fakeSSM{})
	if _, err := r.RunInstance(context.Background(), runSpec{MachineID: "m", InstanceType: "m6i.large", Zone: "us-east-1a", Capacity: "on_demand", IdempotencyToken: "op-1"}); err != nil {
		t.Fatalf("RunInstance: %v", err)
	}
	if e.runInput.InstanceMarketOptions != nil {
		t.Error("on-demand machine should not set InstanceMarketOptions")
	}
}

func TestDescribeManaged_FilterAndMapping(t *testing.T) {
	e := &fakeEC2{describe: &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
		runningInstance("i-1", "us-east-1b", "dns1", []ec2types.Tag{
			{Key: aws.String(tagMachineID), Value: aws.String("slot-9")},
			{Key: aws.String(tagCluster), Value: aws.String("c1")},
			{Key: aws.String(tagCapacity), Value: aws.String("spot")},
		}, true),
	}}}}}
	r := newTestReal(e, &fakeSSM{})

	got, err := r.DescribeManaged(context.Background())
	if err != nil {
		t.Fatalf("DescribeManaged: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d instances, want 1", len(got))
	}
	m := got[0]
	if m.InstanceID != "i-1" || m.MachineID != "slot-9" || m.ClusterID != "c1" || m.Capacity != "spot" || m.Zone != "us-east-1b" || !m.Spot || !m.Running {
		t.Errorf("mapping wrong: %+v", m)
	}
	// The filter must scope to bigfleet:managed=true.
	var hasManaged bool
	for _, fl := range e.descInput[0].Filters {
		if aws.ToString(fl.Name) == "tag:"+tagManaged {
			hasManaged = true
		}
	}
	if !hasManaged {
		t.Error("DescribeManaged filter missing tag:bigfleet:managed")
	}
}

func TestSpotPriceUSD_NewestEntry(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	e := &fakeEC2{spot: &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: []ec2types.SpotPrice{
		{Timestamp: aws.Time(old), SpotPrice: aws.String("0.05")},
		{Timestamp: aws.Time(newer), SpotPrice: aws.String("0.03")}, // newest wins even though listed later/lower
		{Timestamp: nil, SpotPrice: aws.String("9.99")},             // skipped
		{Timestamp: aws.Time(newer), SpotPrice: aws.String("xxx")},  // unparseable, skipped
	}}}
	r := newTestReal(e, &fakeSSM{})
	got, err := r.SpotPriceUSD(context.Background(), "c7g.xlarge", "us-east-1a")
	if err != nil {
		t.Fatalf("SpotPriceUSD: %v", err)
	}
	if got != 0.03 {
		t.Errorf("spot price = %v, want 0.03 (newest valid entry)", got)
	}

	empty := newTestReal(&fakeEC2{spot: &ec2.DescribeSpotPriceHistoryOutput{}}, &fakeSSM{})
	if _, err := empty.SpotPriceUSD(context.Background(), "x", "z"); err == nil {
		t.Error("empty spot history should error")
	}
}

func TestApplyBootstrap_PollsToSuccess(t *testing.T) {
	s := &fakeSSM{status: ssmtypes.CommandInvocationStatusSuccess}
	r := newTestReal(&fakeEC2{}, s)
	if err := r.ApplyBootstrap(context.Background(), "i-1", "cluster-x", []byte("join-data")); err != nil {
		t.Fatalf("ApplyBootstrap: %v", err)
	}
	script := s.sentScript()
	if !strings.Contains(script, "base64 -d") || !strings.Contains(script, "/opt/bigfleet/bootstrap") {
		t.Errorf("bootstrap script missing delivery/hook: %q", script)
	}
}

func TestApplyBootstrap_FailedCommandErrors(t *testing.T) {
	s := &fakeSSM{status: ssmtypes.CommandInvocationStatusFailed, stderr: "hook exited 1"}
	r := newTestReal(&fakeEC2{}, s)
	err := r.ApplyBootstrap(context.Background(), "i-1", "c", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "hook exited 1") {
		t.Errorf("Configure with a failed SSM command must error with stderr, got: %v", err)
	}
}

func TestDrainNode_FailedDrainPropagatesAndNoSwallow(t *testing.T) {
	s := &fakeSSM{status: ssmtypes.CommandInvocationStatusFailed, stderr: "drain timed out"}
	r := newTestReal(&fakeEC2{}, s)
	err := r.DrainNode(context.Background(), "i-1", 30)
	if err == nil {
		t.Fatal("a failed drain must surface as an error, not a false Idle")
	}
	// The drain command itself must not swallow failure (no `drain ... || true`).
	script := s.sentScript()
	if strings.Contains(script, "drain") && strings.Contains(script, "|| true") {
		// cordon may have || true, but not the drain line.
		for _, line := range strings.Split(script, ";") {
			if strings.Contains(line, "kubectl drain") && strings.Contains(line, "|| true") {
				t.Errorf("drain line swallows failure: %q", line)
			}
		}
	}
}

func TestTerminateAndTagOps(t *testing.T) {
	e := &fakeEC2{}
	r := newTestReal(e, &fakeSSM{status: ssmtypes.CommandInvocationStatusSuccess})
	if err := r.TerminateInstance(context.Background(), "i-x"); err != nil {
		t.Fatalf("TerminateInstance: %v", err)
	}
	if len(e.terminated) != 1 || e.terminated[0] != "i-x" {
		t.Errorf("TerminateInstances got %v, want [i-x]", e.terminated)
	}
	// ApplyBootstrap tags the cluster binding; DrainNode removes it.
	_ = r.ApplyBootstrap(context.Background(), "i-x", "c1", []byte("b"))
	if len(e.createTags) != 1 {
		t.Errorf("ApplyBootstrap did not CreateTags the cluster binding")
	}
	_ = r.DrainNode(context.Background(), "i-x", 5)
	if len(e.deleteTags) != 1 {
		t.Errorf("DrainNode did not DeleteTags the cluster binding")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":   "'plain'",
		"a b":     "'a b'",
		"it's":    `'it'\''s'`,
		"$(rm)":   "'$(rm)'",
		"a;b|c&d": "'a;b|c&d'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
