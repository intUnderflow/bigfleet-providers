//go:build certify

package suite

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// C5 — shard_metadata store / echo / clear discipline (behaviors B50x).
//
// The contract (ConfigureRequest.shard_metadata / Machine.shard_metadata, M72):
// the provider stores the map opaquely and echoes it VERBATIM on every
// subsequent Get / List / TransitionAck snapshot until the binding ends (a Drain
// settles to Idle, or a Delete to Speculative). It must never interpret,
// truncate, summarize, re-encode, or alias the caller's bytes; the only sanctioned
// non-echo outcome for a too-large map is an outright InvalidArgument rejection
// under a documented cap (never FAILED_PRECONDITION — that's reserved for
// fencing). These behaviors go far deeper than the upstream echo/clear baseline:
// hostile byte content, byte-for-byte stability across reads and across the List
// projection, clean replacement after a re-bind, the InvalidArgument-or-verbatim
// cap discipline, mid-flight CONFIGURING visibility, and caller-aliasing safety.

// hostileMetadata builds a 55-key map mixing embedded NUL, the full C0/C1
// control range, multibyte unicode, and empty values. proto3 string values must
// be valid UTF-8 (raw invalid bytes can't ride the wire at all, so they are out
// of scope), but valid-UTF-8 NUL and control bytes ARE legal and a conformant
// provider must round-trip them untouched.
func hostileMetadata() map[string]string {
	md := map[string]string{
		"bigfleet.lucy.sh/assigned-priority": "1000000",
		"bigfleet.lucy.sh/assigned-group":    "topology.bigfleet/rack\x00gang-7", // embedded NUL
		"x-unicode/value":                    "παντα ρει — 你好 — 🚀\t\n",
		"x-empty/value":                      "", // empty value must survive
		"x-control/value":                    "\x00\x01\x02\x1f\x7f",
		"x-c1-control/value":                 "", // C1 control range (valid multibyte UTF-8)
		"x-nul-only/value":                   "\x00",
		"x-trailing-space/value":             "padded   ",
		"x-key-with-nul\x00suffix":           "key itself carries a NUL byte",
		"x-mixed/value":                      "a\x00b\x01c\tπ\n你🚀",
	}
	// Pad out to exactly 55 keys with bulk entries of varied lengths.
	for len(md) < 55 {
		i := len(md)
		md[fmt.Sprintf("x-bulk/key-%03d-%s", i, "παδ")] = fmt.Sprintf("v-%d-\x00-%s", i, "你好🚀")
	}
	if len(md) != 55 {
		panic("hostileMetadata must have exactly 55 keys")
	}
	return md
}

// configureRaw drives a Configure carrying an arbitrary metadata map (and an
// optional bootstrap blob), bypassing the harness's fixed-blob Configure so the
// metadata/blob edge cases can be exercised. Returns the ack and error.
func configureRaw(h *harness.H, id, cluster string, md map[string]string, blob []byte) (*pb.TransitionAck, error) {
	ctx, cancel := h.Ctx()
	defer cancel()
	return h.Client.Configure(ctx, &pb.ConfigureRequest{
		MachineId:     id,
		ClusterId:     cluster,
		BootstrapBlob: blob,
		ShardMetadata: md,
	})
}

// B501 — a 55-key map mixing embedded NUL, control bytes, unicode, and empty
// values is echoed byte-for-byte on Get and on List(CONFIGURED), stable across
// repeated reads.
func TestB501_HostileMetadataEchoedVerbatim(t *testing.T) {
	behavior(t, "B501")
	h := dial(t)

	md := hostileMetadata()
	id := h.WalkToConfigured("conf-b501", md)

	// Byte-for-byte on Get, and STABLE across repeated reads (no per-read
	// re-encoding, key reordering with loss, or value drift).
	var first map[string]string
	for i := 0; i < 4; i++ {
		got := h.Get(id).GetShardMetadata()
		if len(got) != len(md) {
			t.Fatalf("read %d: shard_metadata has %d keys, want %d (no truncation/padding)", i, len(got), len(md))
		}
		if !maps.Equal(got, md) {
			t.Fatalf("read %d: shard_metadata not byte-for-byte verbatim", i)
		}
		assertExactBytes(t, fmt.Sprintf("Get read %d", i), md, got)
		if first == nil {
			first = got
		} else if !maps.Equal(got, first) {
			t.Errorf("read %d: shard_metadata drifted from the first read", i)
		}
	}

	// And byte-for-byte through the List(CONFIGURED) projection too. The pool
	// is shared and the fake is long-lived, so the Configured set can carry
	// other (possibly large) machines; use a big-recv-limit client so an
	// unrelated bulky neighbour can't trip the default 4 MiB gRPC ceiling and
	// turn this echo check into a transport error.
	found := false
	for _, m := range listBigRecv(t, pb.MachineState_MACHINE_STATE_CONFIGURED) {
		if m.GetId() != id {
			continue
		}
		found = true
		got := m.GetShardMetadata()
		if !maps.Equal(got, md) {
			t.Errorf("List(CONFIGURED) shard_metadata not verbatim for %s", id)
		}
		assertExactBytes(t, "List(CONFIGURED)", md, got)
	}
	if !found {
		t.Errorf("List(CONFIGURED) did not return %s", id)
	}
}

// listBigRecv issues one List(states...) over a throwaway connection whose
// max-receive size is raised well past the default 4 MiB, so a large shared
// inventory cannot turn a metadata echo assertion into a ResourceExhausted
// transport error. Pure wire: still only the List RPC.
func listBigRecv(t *testing.T, states ...pb.MachineState) []*pb.Machine {
	t.Helper()
	conn, err := grpc.NewClient(target(t),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(128*1024*1024)),
	)
	if err != nil {
		t.Fatalf("listBigRecv dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := pb.NewCapacityProviderClient(conn).List(ctx, &pb.ListFilter{States: states})
	if err != nil {
		t.Fatalf("listBigRecv List: %v", err)
	}
	return resp.GetMachines()
}

// assertExactBytes confirms that for every key in want, got carries the exact
// same value bytes (length + content), and that got has no extra keys — a
// stronger statement than maps.Equal alone for surfacing a precise diff.
func assertExactBytes(t *testing.T, where string, want, got map[string]string) {
	t.Helper()
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("%s: key %q dropped", where, k)
			continue
		}
		if gv != wv {
			t.Errorf("%s: key %q value altered: want %d bytes, got %d bytes", where, k, len(wv), len(gv))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected extra key %q surfaced", where, k)
		}
	}
}

// B502 — shard_metadata and cluster both clear when a Drain settles to Idle, and
// a subsequent Configure with a DISJOINT map shows no key from the prior binding.
func TestB502_ClearedOnDrainAndCleanlyReplaced(t *testing.T) {
	behavior(t, "B502")
	h := dial(t)

	old := map[string]string{
		"old.key/a":   "1",
		"old.key/b":   "2",
		"old.key/nul": "x\x00y",
	}
	id := h.WalkToConfigured("conf-b502-old", old)

	// Sanity: the binding is present while Configured.
	if got := h.Get(id).GetShardMetadata(); !maps.Equal(got, old) {
		t.Fatalf("pre-Drain shard_metadata not as configured")
	}

	// Drain → Idle: cluster and shard_metadata must BOTH clear.
	if _, err := h.Drain(id, 5); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	if len(m.GetShardMetadata()) != 0 {
		t.Errorf("shard_metadata survived Drain→Idle: %d keys remain", len(m.GetShardMetadata()))
	}
	if m.GetCluster() != "" {
		t.Errorf("cluster survived Drain→Idle: %q", m.GetCluster())
	}

	// Re-Configure with a fully DISJOINT key set: not a single old key may leak.
	fresh := map[string]string{
		"new.key/x": "10",
		"new.key/y": "20",
	}
	if _, err := h.Configure(id, "conf-b502-new", fresh); err != nil {
		t.Fatalf("re-Configure: %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
	got := h.Get(id)
	if !maps.Equal(got.GetShardMetadata(), fresh) {
		t.Errorf("re-Configure shard_metadata = %v, want exactly %v", got.GetShardMetadata(), fresh)
	}
	for k := range old {
		if _, leaked := got.GetShardMetadata()[k]; leaked {
			t.Errorf("old binding key %q leaked into the fresh binding", k)
		}
	}
	if got.GetCluster() != "conf-b502-new" {
		t.Errorf("re-Configure cluster = %q, want conf-b502-new", got.GetCluster())
	}
}

// B503 — an oversized / many-key metadata map is either echoed verbatim or
// rejected with InvalidArgument under a documented cap, never silently truncated
// or summarized (and never FAILED_PRECONDITION — that's fencing only). The
// no-partial-transition invariant holds on the reject branch.
func TestB503_OversizedMetadataVerbatimOrInvalidArgument(t *testing.T) {
	behavior(t, "B503")
	h := dial(t)

	// A deliberately large map: many keys AND large values (~2MiB total).
	big := map[string]string{}
	bigVal := makeFiller(8 * 1024) // 8 KiB per value
	for i := 0; i < 256; i++ {
		big[fmt.Sprintf("x-oversize/key-%04d", i)] = bigVal
	}

	id := h.WalkToIdle()

	ack, err := configureRaw(h, id, "conf-b503", big, []byte("# conformance\n"))
	switch harness.Code(err) {
	case codes.OK:
		// Accept branch: it must be echoed VERBATIM, never truncated/summarized.
		h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 30*time.Second)
		got := h.Get(id).GetShardMetadata()
		if len(got) != len(big) {
			t.Errorf("accepted oversized map but echoed %d/%d keys (silent truncation)", len(got), len(big))
		}
		if !maps.Equal(got, big) {
			t.Errorf("accepted oversized map but did not echo it verbatim (truncated/summarized values?)")
		}
		_ = ack
		// Clean up: drain the large binding back to Idle so this ~2 MiB
		// machine does not linger CONFIGURED and bloat unrelated
		// List(CONFIGURED) calls past the gRPC message ceiling (the pool is
		// shared and the fake is long-lived across reruns).
		if _, derr := h.Drain(id, 0); derr == nil {
			h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 30*time.Second)
		}
	case codes.InvalidArgument:
		// Reject branch: the cap rejection must NOT have partially transitioned
		// the machine — it must rest exactly where it was (Idle).
		if s := h.State(id); s != pb.MachineState_MACHINE_STATE_IDLE {
			t.Errorf("oversized-map rejection left machine in %s, want IDLE (no partial transition)", s)
		}
		h.StaysIn(id, pb.MachineState_MACHINE_STATE_IDLE, 300*time.Millisecond)
		if md := h.Get(id).GetShardMetadata(); len(md) != 0 {
			t.Errorf("rejected oversized Configure still wrote %d metadata keys", len(md))
		}
	default:
		t.Errorf("oversized metadata: code %s, want OK (verbatim) or INVALID_ARGUMENT (capped), never %s",
			harness.Code(err), harness.Code(err))
	}
}

// makeFiller returns an n-byte ASCII filler value.
func makeFiller(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return string(b)
}

// B504 — if a CONFIGURING window is observed mid-flight, shard_metadata is
// already visible verbatim in it (best-effort against instant actuators);
// otherwise the verbatim map is asserted once the machine is Configured.
func TestB504_MetadataVisibleInConfiguringOrAtSettle(t *testing.T) {
	behavior(t, "B504")
	h := dial(t)

	md := map[string]string{
		"b504/early":   "must-be-visible-mid-flight",
		"b504/nul":     "x\x00y",
		"b504/unicode": "你好🚀",
	}
	id := h.WalkToIdle()

	if _, err := h.Configure(id, "conf-b504", md); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// Fast-poll the transitional window. If we catch CONFIGURING, the metadata
	// must ALREADY be present verbatim in that snapshot. A fast in-memory
	// actuator may skip the window entirely — that is tolerated.
	sawConfiguring := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		m, err := h.GetRaw(id)
		if err == nil {
			switch m.GetState() {
			case pb.MachineState_MACHINE_STATE_CONFIGURING:
				sawConfiguring = true
				if !maps.Equal(m.GetShardMetadata(), md) {
					t.Errorf("CONFIGURING window did not already carry the verbatim metadata (got %d keys)",
						len(m.GetShardMetadata()))
				}
			case pb.MachineState_MACHINE_STATE_CONFIGURED:
				deadline = time.Now() // settled — break out
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if sawConfiguring {
		t.Logf("observed CONFIGURING mid-flight; metadata was present verbatim")
	} else {
		t.Logf("CONFIGURING window not observed (instant actuator); asserting at settle only")
	}

	// In every case, the settled Configured snapshot carries it verbatim.
	got := h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
	if !maps.Equal(got.GetShardMetadata(), md) {
		t.Errorf("settled CONFIGURED snapshot metadata not verbatim (got %d keys, want %d)",
			len(got.GetShardMetadata()), len(md))
	}
}

// B505 — mutating a metadata map after a Configure ack does not alter the
// provider-stored copy: a fresh Get still returns the originally-sent bytes (no
// caller aliasing). Guards against a provider that retains the caller's map by
// reference rather than copying it on ingest.
func TestB505_NoCallerAliasingAfterConfigure(t *testing.T) {
	behavior(t, "B505")
	h := dial(t)

	// The map we send, and an INDEPENDENT snapshot of its original contents.
	sent := map[string]string{
		"b505/keep":      "original-value",
		"b505/mutate":    "before-mutation",
		"b505/delete-me": "present",
		"b505/nul":       "a\x00b",
	}
	original := maps.Clone(sent)

	id := h.WalkToIdle()
	if _, err := configureRaw(h, id, "conf-b505", sent, []byte("# conformance\n")); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)

	// AFTER the ack, scribble all over the caller's map IN PLACE: mutate a
	// value, delete a key, add a key. A provider that aliased our map would now
	// surface this corruption on the next read.
	sent["b505/mutate"] = "AFTER-MUTATION-POISON"
	delete(sent, "b505/delete-me")
	sent["b505/injected"] = "should-never-appear"

	// A fresh Get must still return the ORIGINAL bytes, untouched.
	got := h.Get(id).GetShardMetadata()
	if !maps.Equal(got, original) {
		t.Errorf("caller aliasing detected: provider copy changed after the caller mutated its own map.\n got=%v\nwant=%v",
			got, original)
	}
	if v := got["b505/mutate"]; v != "before-mutation" {
		t.Errorf("aliasing: mutated value bled through, got %q want %q", v, "before-mutation")
	}
	if _, ok := got["b505/delete-me"]; !ok {
		t.Errorf("aliasing: caller's delete bled through (key vanished from provider copy)")
	}
	if _, ok := got["b505/injected"]; ok {
		t.Errorf("aliasing: caller's injected key bled through into the provider copy")
	}

	// The stored copy must also be self-consistent across a second read.
	if got2 := h.Get(id).GetShardMetadata(); !maps.Equal(got2, original) {
		t.Errorf("second read after mutation also diverged from the original bytes")
	}
}
