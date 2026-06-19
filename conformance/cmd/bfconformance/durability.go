package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// runDurableLane certifies the B10xx DURABILITY / RESTART-RECOVERY behaviors.
//
// Unlike every other lane it is NOT a black-box wire test against a long-lived
// provider: it OWNS the provider lifecycle. It boots the provider under test
// with a --state file (a providerkit.FileStore), drives pre-restart state over
// a raw gRPC client (fence high-water marks, an idempotent Configure, a cluster
// binding + shard_metadata, an inventory snapshot), KILLS the process, then
// boots a FRESH process against the SAME --state path and asserts that every
// piece of durable state survived the restart exactly. Each behavior yields one
// testResult with a B100x marker so it maps onto the registry like the rest.
//
// A failed assertion is recorded as Outcome="fail" on that single behavior — it
// never crashes the lane, so one durability gap does not mask the others.
func runDurableLane(repoRoot, providerName string, port int) ([]testResult, error) {
	tmp, err := os.MkdirTemp("", "bfconformance-durable-")
	if err != nil {
		return nil, fmt.Errorf("durable: mkdtemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	statePath := filepath.Join(tmp, "durable-state.json")

	bin, err := buildProvider(repoRoot, providerName)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	logPath := filepath.Join(tmp, "durable-provider.log")

	// --- PRE-RESTART: boot, drive durable state, snapshot --------------------
	fmt.Fprintf(os.Stderr, ">> [durable] booting %s on %s (seed=64, --state=%s)\n", providerName, addr, statePath)
	prov, err := boot(bin, providerName, addr, 64, logPath, "--state="+statePath)
	if err != nil {
		return nil, err
	}

	pre, perr := drivePreRestart(addr)
	if perr != nil {
		prov.stop()
		return nil, fmt.Errorf("durable: pre-restart drive: %w", perr)
	}
	prov.stop() // KILL — the durable state now lives only in statePath.

	// --- RE-BOOT: fresh process, SAME state file -----------------------------
	// Re-boot on a DIFFERENT port: a SIGKILL'd process can leave its listening
	// socket in TIME_WAIT briefly, so an immediate rebind on the same addr can
	// either fail or (worse) let waitReady connect to the lingering socket. A
	// fresh port makes the restart deterministic; durability lives in --state,
	// not the address.
	addr2 := fmt.Sprintf("127.0.0.1:%d", port+1)
	fmt.Fprintf(os.Stderr, ">> [durable] re-booting %s on %s against the SAME --state\n", providerName, addr2)
	prov2, err := boot(bin, providerName, addr2, 64, logPath, "--state="+statePath)
	if err != nil {
		return nil, fmt.Errorf("durable: re-boot: %w", err)
	}
	defer prov2.stop()

	res := assertPostRestart(addr2, pre)

	// B1006-strong: a dedicated end-to-end recoverInterrupted cycle against the
	// reference faultprovider (which can BLOCK a transition on command, unlike
	// the provider under test's instant fake). This REPLACES the weak,
	// vacuous-against-an-instant-fake B1006 produced by assertPostRestart.
	b1006 := runB1006Recovery(repoRoot, port)
	res = replaceBehavior(res, "B1006", b1006)

	return res, nil
}

// replaceBehavior swaps the testResult carrying behavior id out of res for repl,
// preserving order. If no existing result carries id, repl is appended.
func replaceBehavior(res []testResult, id string, repl testResult) []testResult {
	for i, r := range res {
		if contains(r.Behaviors, id) {
			res[i] = repl
			return res
		}
	}
	return append(res, repl)
}

// faultClusterTimeout is the faultprovider's "block ConfigureInstance until ctx
// is done" selector (its faultBackend's clusterConfigureTO, which lives in the
// separate faultprovider main package — duplicated here as a wire constant).
// Configuring onto it makes the machine sit in CONFIGURING for the whole
// transition timeout, which the B1006 cycle exploits to kill the provider
// mid-transition.
const faultClusterTimeout = "fault-timeout"

// runB1006Recovery exercises providerkit.recoverInterrupted() end-to-end: it
// boots the reference faultprovider against a --state FileStore with a GENEROUS
// --transition-timeout, drives a machine INTO Configuring via the "fault-timeout"
// cluster (whose ConfigureInstance BLOCKS until ctx is done, so the machine sits
// in CONFIGURING), OBSERVES it in CONFIGURING (so the test is non-vacuous),
// KILLS the provider mid-transition, RE-BOOTS against the SAME --state on a fresh
// port, and asserts the orphaned CONFIGURING record was recovered to FAILED with
// a non-empty last_error on reload. Any setup error fails the behavior with a
// clear message rather than silently passing.
//
// Ports: the faultprovider boots on basePort+10 then basePort+11 (the durable
// lane is invoked with port = *basePort+2 and re-boots on +3; the fault lane
// owns +1 and the scale lane +3 — basePort+10/+11 are well clear of all of
// them). The temp --state file and both provider processes are cleaned up on
// every path.
func runB1006Recovery(repoRoot string, basePort int) testResult {
	const id = "B1006"

	bin, err := buildFaultProvider(repoRoot)
	if err != nil {
		return durFail(id, fmt.Sprintf("build faultprovider: %v", err))
	}

	tmp, err := os.MkdirTemp("", "bfconformance-b1006-")
	if err != nil {
		return durFail(id, fmt.Sprintf("mkdtemp: %v", err))
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	statePath := filepath.Join(tmp, "fault-state.json")
	logPath := filepath.Join(tmp, "fault-provider.log")

	// --- boot with a GENEROUS timeout so CONFIGURING persists until we kill it.
	addr := fmt.Sprintf("127.0.0.1:%d", basePort+10)
	fmt.Fprintf(os.Stderr, ">> [durable/B1006] booting faultprovider on %s (--state=%s, --transition-timeout=30s)\n", addr, statePath)
	prov, err := bootFaultProviderWith(bin, addr, logPath, "30s", statePath)
	if err != nil {
		return durFail(id, fmt.Sprintf("boot faultprovider: %v", err))
	}
	// prov is killed below (mid-CONFIGURING); guard a leak on early return.
	provStopped := false
	defer func() {
		if !provStopped {
			prov.stop()
		}
	}()

	c, err := dialDurable(addr)
	if err != nil {
		return durFail(id, fmt.Sprintf("dial faultprovider: %v", err))
	}

	// --- pick a Speculative machine, Create -> IDLE.
	mid, err := c.pickSpeculative(map[string]bool{})
	if err != nil {
		c.close()
		return durFail(id, fmt.Sprintf("pick speculative: %v", err))
	}
	{
		ctx, cancel := ctxTO()
		_, cerr := c.cli.Create(ctx, &pb.CreateRequest{MachineId: mid})
		cancel()
		if cerr != nil {
			c.close()
			return durFail(id, fmt.Sprintf("create %s: %v", mid, cerr))
		}
	}
	if _, err := c.pollState(mid, pb.MachineState_MACHINE_STATE_IDLE, 20*time.Second); err != nil {
		c.close()
		return durFail(id, fmt.Sprintf("machine %s never reached IDLE: %v", mid, err))
	}

	// --- Configure onto "fault-timeout": ConfigureInstance now BLOCKS until ctx
	//     is done, so the machine enters and STAYS in CONFIGURING.
	{
		ctx, cancel := ctxTO()
		_, cerr := c.cli.Configure(ctx, &pb.ConfigureRequest{
			MachineId: mid, ClusterId: faultClusterTimeout,
			BootstrapBlob: []byte("# b1006\n"), ShardMetadata: map[string]string{"b1006/k": "v"},
		})
		cancel()
		if cerr != nil {
			c.close()
			return durFail(id, fmt.Sprintf("configure %s onto %q: %v", mid, faultClusterTimeout, cerr))
		}
	}

	// --- OBSERVE CONFIGURING (bounded). This is what makes the test non-vacuous:
	//     if it never goes CONFIGURING, fail loudly rather than silently pass.
	if _, err := c.pollState(mid, pb.MachineState_MACHINE_STATE_CONFIGURING, 5*time.Second); err != nil {
		c.close()
		return durFail(id, fmt.Sprintf("machine %s never observed in CONFIGURING (the blocking actuator did not engage): %v — test would be vacuous", mid, err))
	}
	fmt.Fprintf(os.Stderr, ">> [durable/B1006] machine %s OBSERVED in CONFIGURING; killing faultprovider mid-transition\n", mid)
	c.close()

	// --- KILL mid-CONFIGURING. The orphaned CONFIGURING record now lives only in
	//     statePath; the goroutine + timeout that would have moved it are gone.
	prov.stop()
	provStopped = true

	// --- RE-BOOT against the SAME --state on a FRESH port (avoid rebind races).
	addr2 := fmt.Sprintf("127.0.0.1:%d", basePort+11)
	logPath2 := filepath.Join(tmp, "fault-provider-2.log")
	fmt.Fprintf(os.Stderr, ">> [durable/B1006] re-booting faultprovider on %s against the SAME --state\n", addr2)
	prov2, err := bootFaultProviderWith(bin, addr2, logPath2, "30s", statePath)
	if err != nil {
		return durFail(id, fmt.Sprintf("re-boot faultprovider: %v", err))
	}
	defer prov2.stop()

	c2, err := dialDurable(addr2)
	if err != nil {
		return durFail(id, fmt.Sprintf("re-dial faultprovider: %v", err))
	}
	defer c2.close()

	// --- ASSERT: recoverInterrupted moved the orphaned CONFIGURING -> FAILED with
	//     a non-empty last_error on reload.
	ctx, cancel := ctxTO()
	m, gerr := c2.cli.Get(ctx, &pb.MachineRef{Id: mid})
	cancel()
	if gerr != nil {
		return durFail(id, fmt.Sprintf("Get %s post-restart: %v", mid, gerr))
	}
	if m.GetState() != pb.MachineState_MACHINE_STATE_FAILED {
		return durFail(id, fmt.Sprintf("machine %s post-restart state=%s, want FAILED — recoverInterrupted did NOT recover the orphaned CONFIGURING record", mid, m.GetState()))
	}
	if m.GetLastError() == "" {
		return durFail(id, fmt.Sprintf("machine %s recovered to FAILED but last_error is empty — recoverInterrupted must surface the interruption", mid))
	}
	fmt.Fprintf(os.Stderr, ">> [durable/B1006] machine %s recovered CONFIGURING->FAILED post-restart (last_error=%q)\n", mid, m.GetLastError())
	return durPass(id, fmt.Sprintf("killed %s mid-CONFIGURING (blocking actuator); on restart recoverInterrupted recovered the orphaned record to FAILED with last_error=%q", mid, m.GetLastError()))
}

// preState captures everything the post-restart assertions compare against.
type preState struct {
	fenceShard  string // shard whose high-water mark we established
	fenceEpoch  int64
	fenceSeq    int64
	idleMachine string // the machine we drove to Idle under the fence

	cfgMachine  string // the machine we Configured
	cfgCluster  string
	cfgMetadata map[string]string
	cfgOpID     string // operation_id Configure returned pre-restart

	inventory map[string]pb.MachineState // full List snapshot (id -> state)
	opIDs     map[string]bool            // every operation_id minted pre-restart
}

// durClient is a raw gRPC client + context helper for the durability lane (the
// harness needs a *testing.T, which the runner does not have).
type durClient struct {
	conn *grpc.ClientConn
	cli  pb.CapacityProviderClient
}

func dialDurable(addr string) (*durClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return &durClient{conn: conn, cli: pb.NewCapacityProviderClient(conn)}, nil
}

func (c *durClient) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

func ctxTO() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// pollState polls Get until the machine reaches want or the deadline elapses.
func (c *durClient) pollState(id string, want pb.MachineState, timeout time.Duration) (pb.MachineState, error) {
	deadline := time.Now().Add(timeout)
	var last pb.MachineState
	for time.Now().Before(deadline) {
		ctx, cancel := ctxTO()
		m, err := c.cli.Get(ctx, &pb.MachineRef{Id: id})
		cancel()
		if err == nil {
			last = m.GetState()
			if last == want {
				return last, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last, fmt.Errorf("machine %s never reached %s (last=%s)", id, want, last)
}

// pickSpeculative returns one Speculative machine id distinct from the given
// exclusions (so the fence machine and the Configure machine differ).
func (c *durClient) pickSpeculative(exclude map[string]bool) (string, error) {
	ctx, cancel := ctxTO()
	defer cancel()
	resp, err := c.cli.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}})
	if err != nil {
		return "", fmt.Errorf("list speculative: %w", err)
	}
	for _, m := range resp.GetMachines() {
		if !exclude[m.GetId()] {
			return m.GetId(), nil
		}
	}
	return "", fmt.Errorf("no spare Speculative machine (have %d, excluded %d)", len(resp.GetMachines()), len(exclude))
}

// drivePreRestart establishes all durable state on a fresh provider and returns
// the snapshot the post-restart assertions compare against.
func drivePreRestart(addr string) (*preState, error) {
	c, err := dialDurable(addr)
	if err != nil {
		return nil, err
	}
	defer c.close()

	pre := &preState{
		cfgMetadata: map[string]string{"d/k": "v", "d/k2": "v2"},
		cfgCluster:  "durable-cluster",
		opIDs:       map[string]bool{},
	}
	used := map[string]bool{}

	// 1. Establish shard "durA"'s high-water mark at (epoch=3, seq=7) via a
	//    fenced Create on a Speculative machine, then poll it to Idle.
	fenceMachine, err := c.pickSpeculative(used)
	if err != nil {
		return nil, err
	}
	used[fenceMachine] = true
	pre.fenceShard, pre.fenceEpoch, pre.fenceSeq = "durA", 3, 7
	pre.idleMachine = fenceMachine
	{
		ctx, cancel := ctxTO()
		ack, err := c.cli.Create(ctx, &pb.CreateRequest{
			MachineId: fenceMachine, ShardId: pre.fenceShard, ShardEpoch: pre.fenceEpoch, SequenceNumber: pre.fenceSeq,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("fenced Create (durA): %w", err)
		}
		if id := ack.GetOperationId(); id != "" {
			pre.opIDs[id] = true
		}
		if _, err := c.pollState(fenceMachine, pb.MachineState_MACHINE_STATE_IDLE, 20*time.Second); err != nil {
			return nil, err
		}
	}

	// 2. Configure a second machine with cluster + shard_metadata; poll to
	//    Configured. Configure goes Speculative->...; the kit requires Idle
	//    first, so walk it to Idle, then Configure (recording its op id).
	cfgMachine, err := c.pickSpeculative(used)
	if err != nil {
		return nil, err
	}
	used[cfgMachine] = true
	pre.cfgMachine = cfgMachine
	{
		ctx, cancel := ctxTO()
		ack, err := c.cli.Create(ctx, &pb.CreateRequest{MachineId: cfgMachine})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("create cfg machine: %w", err)
		}
		if id := ack.GetOperationId(); id != "" {
			pre.opIDs[id] = true
		}
		if _, err := c.pollState(cfgMachine, pb.MachineState_MACHINE_STATE_IDLE, 20*time.Second); err != nil {
			return nil, err
		}
	}
	{
		ctx, cancel := ctxTO()
		ack, err := c.cli.Configure(ctx, &pb.ConfigureRequest{
			MachineId: cfgMachine, ClusterId: pre.cfgCluster,
			BootstrapBlob: []byte("# durable\n"), ShardMetadata: pre.cfgMetadata,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("configure cfg machine: %w", err)
		}
		pre.cfgOpID = ack.GetOperationId()
		if pre.cfgOpID != "" {
			pre.opIDs[pre.cfgOpID] = true
		}
		if _, err := c.pollState(cfgMachine, pb.MachineState_MACHINE_STATE_CONFIGURED, 20*time.Second); err != nil {
			return nil, err
		}
	}

	// 3. Snapshot the full inventory (id -> state).
	pre.inventory, err = c.snapshotInventory()
	if err != nil {
		return nil, err
	}
	return pre, nil
}

// snapshotInventory returns the full id->state map from an unfiltered List.
func (c *durClient) snapshotInventory() (map[string]pb.MachineState, error) {
	ctx, cancel := ctxTO()
	defer cancel()
	resp, err := c.cli.List(ctx, &pb.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("list inventory: %w", err)
	}
	inv := make(map[string]pb.MachineState, len(resp.GetMachines()))
	for _, m := range resp.GetMachines() {
		inv[m.GetId()] = m.GetState()
	}
	return inv, nil
}

// assertPostRestart runs the seven B10xx assertions against the re-booted
// provider, returning one testResult per behavior.
func assertPostRestart(addr string, pre *preState) []testResult {
	c, err := dialDurable(addr)
	if err != nil {
		// If we can't even dial the re-booted provider, fail every behavior
		// rather than crash the whole runner.
		msg := fmt.Sprintf("durable: dial re-booted provider: %v", err)
		var out []testResult
		for _, id := range []string{"B1001", "B1002", "B1003", "B1004", "B1005", "B1006", "B1007"} {
			out = append(out, durFail(id, msg))
		}
		return out
	}
	defer c.close()

	return []testResult{
		c.assertB1001(pre),
		c.assertB1002(pre),
		c.assertB1003(pre),
		c.assertB1004(pre),
		c.assertB1005(pre),
		c.assertB1006(pre),
		c.assertB1007(pre),
	}
}

// B1001: a not-strictly-newer fenced op on shard durA is still rejected with
// FAILED_PRECONDITION — the high-water mark survived the restart.
func (c *durClient) assertB1001(pre *preState) testResult {
	ctx, cancel := ctxTO()
	defer cancel()
	// Re-send the SAME token (epoch=3, seq=7): not strictly newer than the
	// persisted mark, so it must be fenced.
	_, err := c.cli.Create(ctx, &pb.CreateRequest{
		MachineId: pre.idleMachine, ShardId: pre.fenceShard, ShardEpoch: pre.fenceEpoch, SequenceNumber: pre.fenceSeq,
	})
	if status.Code(err) == codes.FailedPrecondition {
		return durPass("B1001", fmt.Sprintf("stale token (%d,%d) on shard %s rejected FAILED_PRECONDITION post-restart (mark survived)",
			pre.fenceEpoch, pre.fenceSeq, pre.fenceShard))
	}
	return durFail("B1001", fmt.Sprintf("stale token (%d,%d) on shard %s should be FAILED_PRECONDITION post-restart; got %v (code=%s) — high-water mark was NOT persisted",
		pre.fenceEpoch, pre.fenceSeq, pre.fenceShard, err, status.Code(err)))
}

// B1002: an idempotent retry of the pre-restart Configure (same machine, same
// target) returns the SAME operation_id — the idempotency map survived.
func (c *durClient) assertB1002(pre *preState) testResult {
	ctx, cancel := ctxTO()
	defer cancel()
	ack, err := c.cli.Configure(ctx, &pb.ConfigureRequest{
		MachineId: pre.cfgMachine, ClusterId: pre.cfgCluster,
		BootstrapBlob: []byte("# durable\n"), ShardMetadata: pre.cfgMetadata,
	})
	if err != nil {
		return durFail("B1002", fmt.Sprintf("idempotent Configure retry on %s errored: %v", pre.cfgMachine, err))
	}
	got := ack.GetOperationId()
	if got == pre.cfgOpID && got != "" {
		return durPass("B1002", fmt.Sprintf("idempotent Configure retry returned the same operation_id %q (idempotency map survived)", got))
	}
	return durFail("B1002", fmt.Sprintf("idempotent Configure retry returned operation_id %q, want pre-restart %q — idempotency map was NOT persisted", got, pre.cfgOpID))
}

// B1003: the Configured machine still reports cluster + verbatim shard_metadata
// — the binding survived.
func (c *durClient) assertB1003(pre *preState) testResult {
	ctx, cancel := ctxTO()
	defer cancel()
	m, err := c.cli.Get(ctx, &pb.MachineRef{Id: pre.cfgMachine})
	if err != nil {
		return durFail("B1003", fmt.Sprintf("Get %s post-restart errored: %v", pre.cfgMachine, err))
	}
	if m.GetCluster() != pre.cfgCluster {
		return durFail("B1003", fmt.Sprintf("cluster=%q post-restart, want %q — binding NOT persisted", m.GetCluster(), pre.cfgCluster))
	}
	if !reflect.DeepEqual(m.GetShardMetadata(), pre.cfgMetadata) {
		return durFail("B1003", fmt.Sprintf("shard_metadata=%v post-restart, want %v — metadata NOT persisted", m.GetShardMetadata(), pre.cfgMetadata))
	}
	return durPass("B1003", fmt.Sprintf("Configured machine %s still reports cluster=%q and verbatim shard_metadata (binding survived)", pre.cfgMachine, pre.cfgCluster))
}

// B1004: full inventory (id -> state) equals the pre-restart snapshot — no
// machine lost or duplicated.
func (c *durClient) assertB1004(pre *preState) testResult {
	post, err := c.snapshotInventory()
	if err != nil {
		return durFail("B1004", fmt.Sprintf("List inventory post-restart errored: %v", err))
	}
	if len(post) != len(pre.inventory) {
		return durFail("B1004", fmt.Sprintf("inventory size %d post-restart, want %d — machine(s) lost or duplicated", len(post), len(pre.inventory)))
	}
	var diffs []string
	for id, want := range pre.inventory {
		got, ok := post[id]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("missing %s", id))
			continue
		}
		if got != want {
			diffs = append(diffs, fmt.Sprintf("%s: %s->%s", id, want, got))
		}
	}
	for id := range post {
		if _, ok := pre.inventory[id]; !ok {
			diffs = append(diffs, fmt.Sprintf("extra %s", id))
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		return durFail("B1004", fmt.Sprintf("inventory diverged post-restart: %v", diffs))
	}
	return durPass("B1004", fmt.Sprintf("full inventory (%d machines, id->state) recovered identically post-restart", len(post)))
}

// B1005: a post-restart List revision is usable AND a freshly minted
// operation_id (from a new mutation on a fresh machine) is not reused from any
// pre-restart cycle — freshness, not counter monotonicity.
func (c *durClient) assertB1005(pre *preState) testResult {
	// Revision must be non-empty/usable.
	rctx, rcancel := ctxTO()
	resp, err := c.cli.List(rctx, &pb.ListFilter{})
	rcancel()
	if err != nil {
		return durFail("B1005", fmt.Sprintf("post-restart List errored: %v", err))
	}
	if len(resp.GetRevision()) == 0 {
		return durFail("B1005", "post-restart List revision is empty — not a usable since-cursor")
	}

	// Mint a fresh operation_id from a new mutation on a fresh Speculative
	// machine; it must differ from every pre-restart operation_id.
	used := map[string]bool{pre.idleMachine: true, pre.cfgMachine: true}
	fresh, err := c.pickSpeculative(used)
	if err != nil {
		return durFail("B1005", fmt.Sprintf("no fresh Speculative machine for a new mutation: %v", err))
	}
	mctx, mcancel := ctxTO()
	ack, err := c.cli.Create(mctx, &pb.CreateRequest{MachineId: fresh})
	mcancel()
	if err != nil {
		return durFail("B1005", fmt.Sprintf("fresh Create on %s errored: %v", fresh, err))
	}
	newOp := ack.GetOperationId()
	if newOp == "" {
		return durFail("B1005", "fresh Create returned an empty operation_id")
	}
	if pre.opIDs[newOp] {
		return durFail("B1005", fmt.Sprintf("fresh operation_id %q collides with a pre-restart operation_id — op counter was NOT persisted (reuse across restart)", newOp))
	}
	return durPass("B1005", fmt.Sprintf("post-restart revision usable (%q) and fresh operation_id %q distinct from all %d pre-restart ids",
		string(resp.GetRevision()), newOp, len(pre.opIDs)))
}

// B1006 (weak invariant): no machine is stuck in a transitional state after
// restart. Against the provider-under-test's instant fake every drive settled
// before the kill, so this only asserts the negative ("nothing transitional
// post-restart") — it never actually drives recoverInterrupted. runDurableLane
// REPLACES this result with runB1006Recovery's strong, non-vacuous cycle
// (kill mid-CONFIGURING against the blocking faultprovider, assert FAILED on
// reload). This weak check is still computed as a cheap negative sanity guard on
// the provider under test; its result is swapped out before reporting.
func (c *durClient) assertB1006(pre *preState) testResult {
	post, err := c.snapshotInventory()
	if err != nil {
		return durFail("B1006", fmt.Sprintf("List inventory post-restart errored: %v", err))
	}
	var stuck []string
	for id, st := range post {
		if isTransitional(st) {
			stuck = append(stuck, fmt.Sprintf("%s=%s", id, st))
		}
	}
	if len(stuck) > 0 {
		sort.Strings(stuck)
		return durFail("B1006", fmt.Sprintf("machine(s) stuck in a transitional state post-restart: %v (kit reload did not recover them)", stuck))
	}
	return durPass("B1006", "no machine is stuck in a transitional state post-restart (kit recovers interrupted transitions on reload)")
}

// B1007: a brand-new shard "durB"'s first low token (1,1) is ACCEPTED — fence
// marks are per-shard, with no global high-water collapse from the restart.
func (c *durClient) assertB1007(pre *preState) testResult {
	// Use a fresh Speculative machine so the call's only gate is the fence.
	used := map[string]bool{pre.idleMachine: true, pre.cfgMachine: true}
	fresh, err := c.pickSpeculative(used)
	if err != nil {
		return durFail("B1007", fmt.Sprintf("no fresh Speculative machine for the durB probe: %v", err))
	}
	ctx, cancel := ctxTO()
	defer cancel()
	_, err = c.cli.Create(ctx, &pb.CreateRequest{
		MachineId: fresh, ShardId: "durB", ShardEpoch: 1, SequenceNumber: 1,
	})
	if status.Code(err) == codes.FailedPrecondition {
		return durFail("B1007", "brand-new shard durB first token (1,1) was rejected FAILED_PRECONDITION — a GLOBAL high-water mark survived the restart instead of per-shard isolation")
	}
	if err != nil {
		return durFail("B1007", fmt.Sprintf("durB first token (1,1) errored unexpectedly: %v (code=%s)", err, status.Code(err)))
	}
	return durPass("B1007", "brand-new shard durB first token (1,1) accepted post-restart (per-shard mark isolation survived; no global high-water mark)")
}

func isTransitional(s pb.MachineState) bool {
	switch s {
	case pb.MachineState_MACHINE_STATE_CREATING,
		pb.MachineState_MACHINE_STATE_CONFIGURING,
		pb.MachineState_MACHINE_STATE_DRAINING,
		pb.MachineState_MACHINE_STATE_DELETING:
		return true
	default:
		return false
	}
}

// durPass / durFail build a single-behavior testResult carrying its BEHAVIOR
// marker (so report.go maps it onto the registry) plus a human message.
func durResult(id, outcome, msg string) testResult {
	return testResult{
		Pkg:       "durable",
		Name:      "TestB" + id[1:] + "_RestartRecovery",
		Outcome:   outcome,
		Behaviors: []string{id},
		Output:    fmt.Sprintf("BEHAVIOR %s\n%s\n", id, msg),
	}
}

func durPass(id, msg string) testResult { return durResult(id, "pass", msg) }
func durFail(id, msg string) testResult { return durResult(id, "fail", msg) }
