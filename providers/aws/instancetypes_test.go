package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestInstanceCapacityAllocatable(t *testing.T) {
	cases := []struct {
		name string
		cap  instanceCapacity
		want map[string]string
	}{
		{"whole GiB renders Gi", instanceCapacity{VCPU: 2, MemMiB: gib(8)}, map[string]string{"cpu": "2", "memory": "8Gi"}},
		{"half GiB renders Mi", instanceCapacity{VCPU: 2, MemMiB: 512}, map[string]string{"cpu": "2", "memory": "512Mi"}},
		{"1.5 GiB renders Mi", instanceCapacity{VCPU: 4, MemMiB: 1536}, map[string]string{"cpu": "4", "memory": "1536Mi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cap.allocatable(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("allocatable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// capStubEC2 is an ec2Client whose DescribeInstanceCapacities returns a fixed
// table (or error); all other methods come from the embedded ec2Fake.
type capStubEC2 struct {
	*ec2Fake
	caps  map[string]instanceCapacity
	err   error
	calls [][]string
}

func (c *capStubEC2) DescribeInstanceCapacities(_ context.Context, types []string) (map[string]instanceCapacity, error) {
	c.calls = append(c.calls, types)
	if c.err != nil {
		return nil, c.err
	}
	out := make(map[string]instanceCapacity, len(types))
	for _, t := range types {
		if v, ok := c.caps[t]; ok {
			out[t] = v
		}
	}
	return out, nil
}

func TestInstanceTypeResolver_StaticSeedAndUnknown(t *testing.T) {
	r := newInstanceTypeResolver(newEC2Fake(), quietLogger())
	// Pinned type resolves from the seed without any AWS call.
	if got := r.allocatable("m6i.large"); !reflect.DeepEqual(got, map[string]string{"cpu": "2", "memory": "8Gi"}) {
		t.Fatalf("pinned m6i.large = %v", got)
	}
	// Unknown type yields nil (engine treats allocatable == resources).
	if got := r.allocatable("m6i.16xlarge"); got != nil {
		t.Fatalf("unknown type should be nil, got %v", got)
	}
}

func TestInstanceTypeResolver_OverlaysAWSTruth(t *testing.T) {
	stub := &capStubEC2{
		ec2Fake: newEC2Fake(),
		caps: map[string]instanceCapacity{
			"m6i.16xlarge": {VCPU: 64, MemMiB: gib(256)}, // new type, not pinned
			"m6i.large":    {VCPU: 2, MemMiB: gib(8)},    // pinned, AWS agrees
		},
	}
	r := newInstanceTypeResolver(stub, quietLogger())

	missing := r.resolve(context.Background(), []string{"m6i.16xlarge", "m6i.large", "bogus.type", "m6i.large"})
	if missing != 1 { // only bogus.type is unresolved
		t.Fatalf("missing = %d, want 1", missing)
	}
	// The resolver deduped + sorted before the single call.
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 DescribeInstanceCapacities call, got %d", len(stub.calls))
	}
	if got := stub.calls[0]; !reflect.DeepEqual(got, []string{"bogus.type", "m6i.16xlarge", "m6i.large"}) {
		t.Fatalf("call args not deduped/sorted: %v", got)
	}
	// The new type now resolves from AWS truth.
	if got := r.allocatable("m6i.16xlarge"); !reflect.DeepEqual(got, map[string]string{"cpu": "64", "memory": "256Gi"}) {
		t.Fatalf("m6i.16xlarge after resolve = %v", got)
	}
	// The unresolved type stays nil.
	if got := r.allocatable("bogus.type"); got != nil {
		t.Fatalf("bogus.type should stay nil, got %v", got)
	}
}

func TestInstanceTypeResolver_ErrorKeepsPinnedFallback(t *testing.T) {
	stub := &capStubEC2{ec2Fake: newEC2Fake(), err: errors.New("throttled")}
	r := newInstanceTypeResolver(stub, quietLogger())

	missing := r.resolve(context.Background(), []string{"m6i.large", "c7g.xlarge"})
	if missing != 2 {
		t.Fatalf("missing = %d, want 2 (both unresolved on error)", missing)
	}
	// Pinned values survive the failed resolve.
	if got := r.allocatable("m6i.large"); !reflect.DeepEqual(got, map[string]string{"cpu": "2", "memory": "8Gi"}) {
		t.Fatalf("pinned fallback lost after error: %v", got)
	}
}

func TestDedupeNonEmpty(t *testing.T) {
	got := dedupeNonEmpty([]string{"b", "a", "a", "", "b", "c"})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("dedupeNonEmpty = %v", got)
	}
	if got := dedupeNonEmpty(nil); got != nil {
		t.Fatalf("dedupeNonEmpty(nil) = %v, want nil", got)
	}
}
