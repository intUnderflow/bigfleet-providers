// Package providerkit is the shared correctness library every BigFleet
// capacity provider builds on.
//
// A BigFleet provider is a gRPC server implementing
// bigfleet.v1alpha1.CapacityProvider — six RPCs (Create, Configure, Drain,
// Delete, Get, List) that walk machines through a fixed lifecycle on some
// substrate (AWS, GCP, libvirt, bare metal, …). Every provider owes the
// *same* cross-cutting obligations, and getting any of them wrong is a
// correctness incident, not a cosmetic bug:
//
//   - Async dispatch: the four lifecycle RPCs return a TransitionAck
//     immediately and do the real work in the background; progress is
//     observed via Get/List.
//   - Idempotency: a repeated lifecycle call for the same machine and
//     operation returns the same operation_id, and the mapping survives a
//     restart.
//   - Transition timeouts: a transition that does not complete in time
//     lands the machine in MACHINE_STATE_FAILED with last_error set.
//   - Fencing (BigFleet paper §11): every mutating RPC carries a
//     (shard_id, shard_epoch, sequence_number) token; the provider keeps a
//     per-shard high-water mark and rejects any not-strictly-newer token
//     with FAILED_PRECONDITION *without applying it*, checking the fence
//     before the idempotency short-circuit. FAILED_PRECONDITION is reserved
//     for fencing on this service.
//   - Field shape: instance_type / zone / capacity_type are top-level
//     fields, never buried in labels; interruption_probability is always a
//     real value for SPOT.
//   - shard_metadata: stored verbatim from Configure, echoed byte-for-byte
//     on every snapshot, and cleared together with the cluster binding when
//     a Drain completes.
//
// providerkit centralises all of that. A provider author implements the
// small, substrate-specific [Backend] interface and gets the whole
// cross-cutting contract for free via [Server], which is a real
// pb.CapacityProviderServer. The authoritative contract lives in the
// bigfleet repo: api/proto/bigfleet/v1alpha1/provider.proto and
// docs/provider-author-guide.md. This package is the executable mirror of
// that guide's "common mistakes" list — so providers cannot make them.
//
// The proto contract is consumed from the bigfleet Go module
// (github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1); it is
// never re-generated or vendored here, so the wire contract can never
// drift.
package providerkit
