package providerkit

import (
	"context"
	"fmt"
	"math"
)

// Backend is the substrate-specific half of a provider — the *only* thing a
// provider author writes. The kit ([Server]) wraps it with every
// cross-cutting BigFleet obligation: fencing, idempotency, async dispatch,
// transition timeouts, the shard_metadata lifecycle, and Machine
// field-shape validation. A Backend must never re-implement any of those;
// it speaks pure substrate.
//
// The four actuator methods are synchronous from the backend's point of
// view: the kit calls them on a background goroutine, under a per-transition
// timeout, and translates their return into the observable state machine.
// Return promptly on ctx cancellation — the kit cancels ctx when a
// transition's timeout fires, and a machine whose actuator overruns lands in
// MACHINE_STATE_FAILED.
type Backend interface {
	// Describe returns the backend's current substrate inventory. The kit
	// calls it once at startup to seed the authoritative store (when the
	// store is empty), and again from [Server.Reconcile]. Return substrate
	// truth only — id, host, instance_type, zone, capacity_type,
	// price_per_hour, interruption_probability, resources, allocatable,
	// labels, and a State hint of Speculative (a quota slot, host empty) or
	// Idle/Configured (a host that already exists, e.g. a bare-metal free
	// pool). Lifecycle state, the cluster binding and shard_metadata are the
	// kit's job and must not be set here.
	Describe(ctx context.Context) ([]Instance, error)

	// CreateInstance actuates a Speculative slot into a real, Idle host. The
	// kit has already moved the record to Creating; on success it moves it
	// to Idle, attaching the returned HostRef (and folding in any refined
	// substrate facts). Return an error to drive the machine to Failed.
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (CreateInstanceResult, error)

	// ConfigureInstance injects the bootstrap blob and binds the host to a
	// cluster. The kit has already moved the record to Configuring; on
	// success it moves it to Configured, recording the cluster and
	// shard_metadata. The bootstrap blob is opaque — apply it as user-data
	// / ignition / PXE; never parse it.
	ConfigureInstance(ctx context.Context, req ConfigureInstanceRequest) error

	// DrainInstance returns a Configured host to Idle, honouring the grace
	// period for graceful node shutdown. The kit has already moved the
	// record to Draining; on success it moves it to Idle and clears the
	// cluster + shard_metadata.
	DrainInstance(ctx context.Context, req DrainInstanceRequest) error
}

// Deleter is the optional Delete capability. A Backend that can tear a host
// down (cloud) also implements Deleter; a bare-metal free-pool provider
// simply omits it, and the kit answers Delete with codes.Unimplemented —
// which the shard treats as "this provider does not delete" (its M73
// idle-release path never emits Delete for fixed capacity anyway). This is
// the idiomatic realisation of "Delete is optional": the method is either
// implemented on the backend type or it isn't.
type Deleter interface {
	// DeleteInstance tears a host down, returning it to a Speculative quota
	// slot. The kit has already moved the record to Deleting; on success it
	// moves it to Speculative, clearing the host.
	DeleteInstance(ctx context.Context, req DeleteInstanceRequest) error
}

// Instance is the substrate truth the Backend reports for one machine via
// Describe. It carries no lifecycle/binding/metadata bookkeeping — only what
// the substrate knows.
type Instance struct {
	ID   string
	Host HostRef

	// State is a hint used only when the kit has no persisted record for
	// this id. Describe reports substrate truth, so the only valid values are
	// Speculative (a quota slot, Host empty) and Idle (a host that already
	// exists, e.g. a bare-metal free pool, Host set). StateUnspecified is
	// resolved to Speculative when Host is empty and Idle when Host is set.
	// Lifecycle/binding states (Configuring, Configured, Draining, …) are the
	// kit's job and are reloaded from the store on restart — a persisted
	// record always wins — so Describe must not report them.
	State State

	InstanceType            string
	Zone                    string
	CapacityType            CapacityType
	PricePerHour            float64
	InterruptionProbability float64
	Resources               map[string]string
	Allocatable             map[string]string
	Labels                  map[string]string
}

// CreateInstanceRequest is handed to Backend.CreateInstance.
type CreateInstanceRequest struct {
	// Machine is a copy of the record as the kit holds it at dispatch time,
	// already moved to Creating. Use its substrate fields (instance_type,
	// zone, capacity_type, …) as the spec for what to provision.
	Machine Machine
	// OperationID is the kit's idempotency key for this transition: stable
	// across retries of the same operation, fresh for a new operation cycle
	// (so a Speculative slot that is Created, Deleted, then Created again gets
	// a new id). A backend that actuates a non-idempotent substrate call
	// SHOULD use this as the substrate's idempotency token — e.g. EC2
	// RunInstances ClientToken — so a retried Create cannot double-provision,
	// while a genuine re-Create after a delete still launches fresh.
	OperationID string
}

// CreateInstanceResult is returned by Backend.CreateInstance.
type CreateInstanceResult struct {
	// Host is the reference the freshly created host is reachable at. The
	// kit attaches it when the record settles at Idle.
	Host HostRef
	// Resources / Allocatable optionally refine the substrate facts once the
	// real host reports them (an instance may expose more precise capacity
	// than the speculative spec predicted). Leave nil to keep the seeded
	// values.
	Resources   map[string]string
	Allocatable map[string]string
}

// ConfigureInstanceRequest is handed to Backend.ConfigureInstance.
type ConfigureInstanceRequest struct {
	Machine       Machine
	ClusterID     string
	BootstrapBlob []byte
	// OperationID is the per-operation idempotency key — see
	// CreateInstanceRequest.OperationID. Usable as a substrate idempotency
	// token (e.g. an SSM command idempotency key).
	OperationID string
}

// DrainInstanceRequest is handed to Backend.DrainInstance.
type DrainInstanceRequest struct {
	Machine            Machine
	GracePeriodSeconds int64
	// OperationID is the per-operation idempotency key — see
	// CreateInstanceRequest.OperationID.
	OperationID string
}

// DeleteInstanceRequest is handed to Backend.DeleteInstance.
type DeleteInstanceRequest struct {
	Machine Machine
	// OperationID is the per-operation idempotency key — see
	// CreateInstanceRequest.OperationID.
	OperationID string
}

// validate enforces the Machine field-shape contract the autoscaler depends
// on. It is applied at seed/Describe time so every record the kit ever emits
// is in shape; the kit's own transitions only change state/host/binding and
// never blank these fields.
//
// Hard requirements (always rejected):
//   - instance_type must be set (the shard satisfies instance-type selectors
//     from this field directly, never from labels)
//   - capacity_type must be set (drives idle-hold policy and effective cost)
//   - price_per_hour must be ≥ 0 and not NaN
//   - interruption_probability must lie in [0, 1] and not be NaN
//   - a SPOT machine must declare a real (> 0) interruption_probability —
//     effective_cost = price + probability × penalty, so a 0 here lets a
//     spot machine win high-penalty workloads it should never get
//
// zone is required only when requireZone is set (multi-zone providers);
// single-zone providers legitimately omit it, and the conformance suite
// permits an empty zone.
func (in Instance) validate(requireZone bool) error {
	if in.ID == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidMachine)
	}
	if in.InstanceType == "" {
		return fmt.Errorf("%w: %s: instance_type is required (never labels-only)", ErrInvalidMachine, in.ID)
	}
	if in.CapacityType == CapacityUnspecified {
		return fmt.Errorf("%w: %s: capacity_type is required", ErrInvalidMachine, in.ID)
	}
	if requireZone && in.Zone == "" {
		return fmt.Errorf("%w: %s: zone is required for multi-zone providers", ErrInvalidMachine, in.ID)
	}
	if math.IsNaN(in.PricePerHour) || in.PricePerHour < 0 {
		return fmt.Errorf("%w: %s: price_per_hour %v must be ≥ 0 and not NaN", ErrInvalidMachine, in.ID, in.PricePerHour)
	}
	if math.IsNaN(in.InterruptionProbability) || in.InterruptionProbability < 0 || in.InterruptionProbability > 1 {
		return fmt.Errorf("%w: %s: interruption_probability %v must be in [0,1]", ErrInvalidMachine, in.ID, in.InterruptionProbability)
	}
	if in.CapacityType == CapacitySpot && in.InterruptionProbability <= 0 {
		return fmt.Errorf("%w: %s: SPOT machine must declare a real interruption_probability (> 0)", ErrInvalidMachine, in.ID)
	}
	// Host-vs-state invariant: the proto/guide require host to be null for
	// SPECULATIVE and set otherwise. Describe may only report the two
	// substrate-truth resting states (Speculative, Idle); anything else is a
	// lifecycle/binding state the kit owns and the store reloads.
	switch in.effectiveState() {
	case StateSpeculative:
		if !in.Host.empty() {
			return fmt.Errorf("%w: %s: a Speculative machine must not carry a host", ErrInvalidMachine, in.ID)
		}
	case StateIdle:
		if in.Host.empty() {
			return fmt.Errorf("%w: %s: an Idle machine must carry a host (host is set for every non-Speculative state)", ErrInvalidMachine, in.ID)
		}
	default:
		return fmt.Errorf("%w: %s: Describe may only report Speculative or Idle machines, got %s (lifecycle/binding state is the kit's job)", ErrInvalidMachine, in.ID, in.State)
	}
	return nil
}

// effectiveState resolves the State hint the way toMachine does: an unset hint
// becomes Speculative when there is no host and Idle when there is one.
func (in Instance) effectiveState() State {
	if in.State != StateUnspecified {
		return in.State
	}
	if in.Host.empty() {
		return StateSpeculative
	}
	return StateIdle
}

// toMachine materialises a validated Instance into a fresh kit Machine
// record, resolving the State hint. It is only used for ids the kit does not
// already track.
func (in Instance) toMachine() *Machine {
	st := in.effectiveState()
	return &Machine{
		ID:                      in.ID,
		State:                   st,
		Host:                    in.Host,
		InstanceType:            in.InstanceType,
		Zone:                    in.Zone,
		CapacityType:            in.CapacityType,
		PricePerHour:            in.PricePerHour,
		InterruptionProbability: in.InterruptionProbability,
		Resources:               cloneMap(in.Resources),
		Allocatable:             cloneMap(in.Allocatable),
		Labels:                  cloneMap(in.Labels),
	}
}
