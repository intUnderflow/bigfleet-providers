package providerkit

import "errors"

// Sentinel errors the kit maps to gRPC status codes (see server.go's
// mapErr). Backends return ErrDeleteUnsupported to opt out of Delete; the
// rest are produced by the kit itself.
var (
	// ErrNotFound is returned for an unknown machine id. Maps to
	// codes.NotFound.
	ErrNotFound = errors.New("providerkit: machine not found")

	// ErrFenced is returned when a mutating RPC's token is not strictly
	// newer than the per-shard high-water mark. Maps to
	// codes.FailedPrecondition — the only use of that code on this service,
	// so the shard can alert on zombie-shard incidents mechanically.
	ErrFenced = errors.New("providerkit: request fenced (stale shard token)")

	// ErrInvalidTransition is returned for an out-of-position lifecycle
	// call (e.g. Drain on a Speculative machine, Delete on a Configured
	// one). Maps to codes.Internal — deliberately NOT
	// codes.FailedPrecondition, which is reserved for fencing.
	ErrInvalidTransition = errors.New("providerkit: invalid state transition")

	// ErrInvalidMachine is returned by validation when a record is missing
	// a required substrate field (instance_type, capacity_type) or carries
	// an out-of-bounds cost input. Maps to codes.Internal when it surfaces
	// on an RPC path; usually it is caught at seed time.
	ErrInvalidMachine = errors.New("providerkit: invalid machine record")
)
