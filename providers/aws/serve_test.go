package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMetricsEC2Client_RecordsAndTransparent(t *testing.T) {
	m := newMetrics()
	c := newMetricsEC2Client(newEC2Fake(), m)

	inst, err := c.RunInstance(context.Background(), runSpec{MachineID: "m", InstanceType: "m6i.large", Zone: "us-east-1a"})
	if err != nil || inst.InstanceID == "" {
		t.Fatalf("RunInstance transparent failure: %v %+v", err, inst)
	}
	if got := testutil.ToFloat64(m.ec2Calls.WithLabelValues("RunInstances", "success")); got != 1 {
		t.Errorf("ec2_api_calls{RunInstances,success} = %v, want 1", got)
	}
	// An error outcome is recorded too.
	if err := c.TerminateInstance(context.Background(), "i-nope"); err == nil {
		t.Fatal("expected terminate-unknown error")
	}
	if got := testutil.ToFloat64(m.ec2Calls.WithLabelValues("TerminateInstances", "error")); got != 1 {
		t.Errorf("ec2_api_calls{TerminateInstances,error} = %v, want 1", got)
	}
}

func TestRecoveryInterceptor_PanicBecomesInternal(t *testing.T) {
	m := newMetrics()
	ic := recoveryInterceptor(m, quietLogger())
	_, err := ic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/svc/Create"},
		func(context.Context, any) (any, error) { panic("boom") })
	if status.Code(err) != codes.Internal {
		t.Errorf("panic mapped to %s, want Internal", status.Code(err))
	}
	if testutil.ToFloat64(m.panics) != 1 {
		t.Error("recovered panic not counted")
	}
}

func TestLoggingInterceptor_RecordsRPCMetrics(t *testing.T) {
	m := newMetrics()
	ic := loggingInterceptor(m, quietLogger())
	_, _ = ic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/bigfleet.v1alpha1.CapacityProvider/Create"},
		func(context.Context, any) (any, error) { return "ok", nil })
	if got := testutil.ToFloat64(m.rpcCalls.WithLabelValues("Create", "OK")); got != 1 {
		t.Errorf("grpc_requests{Create,OK} = %v, want 1", got)
	}
}

func TestShortMethod(t *testing.T) {
	if got := shortMethod("/bigfleet.v1alpha1.CapacityProvider/Drain"); got != "Drain" {
		t.Errorf("shortMethod = %q, want Drain", got)
	}
}
