package providerkit

// State is a machine's lifecycle state. It mirrors the proto MachineState
// enum but is kept as a kit-local type so the providerkit state machine and
// store are independent of the wire encoding. Three states are stable
// (Speculative, Idle, Configured), four are transitional (Creating,
// Configuring, Draining, Deleting), and one is terminal-pending-cleanup
// (Failed).
type State uint8

const (
	StateUnspecified State = iota
	StateSpeculative       // quota slot; no real host, no cluster
	StateCreating          // Speculative → Idle in progress
	StateIdle              // real host, no cluster binding
	StateConfiguring       // Idle → Configured in progress
	StateConfigured        // real host bound to a cluster
	StateDraining          // Configured → Idle in progress
	StateDeleting          // Idle → Speculative in progress
	StateFailed            // last transition timed out / errored; needs intervention
)

func (s State) String() string {
	switch s {
	case StateSpeculative:
		return "Speculative"
	case StateCreating:
		return "Creating"
	case StateIdle:
		return "Idle"
	case StateConfiguring:
		return "Configuring"
	case StateConfigured:
		return "Configured"
	case StateDraining:
		return "Draining"
	case StateDeleting:
		return "Deleting"
	case StateFailed:
		return "Failed"
	default:
		return "Unspecified"
	}
}

// MarshalText / UnmarshalText make State persist as its name, so the store
// file is human-readable and resilient to enum reordering.
func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s *State) UnmarshalText(b []byte) error {
	switch string(b) {
	case "Speculative":
		*s = StateSpeculative
	case "Creating":
		*s = StateCreating
	case "Idle":
		*s = StateIdle
	case "Configuring":
		*s = StateConfiguring
	case "Configured":
		*s = StateConfigured
	case "Draining":
		*s = StateDraining
	case "Deleting":
		*s = StateDeleting
	case "Failed":
		*s = StateFailed
	default:
		*s = StateUnspecified
	}
	return nil
}

// CapacityType is the cost-of-holding category. It drives idle-hold policy
// and the effective-cost formula on the shard side, so a provider must
// declare it honestly.
type CapacityType uint8

const (
	CapacityUnspecified CapacityType = iota
	CapacityBareMetal
	CapacityReserved
	CapacityOnDemand
	CapacitySpot
)

func (c CapacityType) String() string {
	switch c {
	case CapacityBareMetal:
		return "BareMetal"
	case CapacityReserved:
		return "Reserved"
	case CapacityOnDemand:
		return "OnDemand"
	case CapacitySpot:
		return "Spot"
	default:
		return "Unspecified"
	}
}

// MarshalText / UnmarshalText make CapacityType persist as its name.
func (c CapacityType) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

func (c *CapacityType) UnmarshalText(b []byte) error {
	switch string(b) {
	case "BareMetal":
		*c = CapacityBareMetal
	case "Reserved":
		*c = CapacityReserved
	case "OnDemand":
		*c = CapacityOnDemand
	case "Spot":
		*c = CapacitySpot
	default:
		*c = CapacityUnspecified
	}
	return nil
}

// HostRef identifies a real host on the substrate. It is empty while a
// machine is Speculative or Creating (no host exists yet).
type HostRef struct {
	// Provider is the provider/region name, used in logs (e.g.
	// "aws-eu-west-1", "bare-metal-amsterdam").
	Provider string
	// Ref is the substrate-scoped host identifier (instance id, BMC
	// serial, …).
	Ref string
}

func (h HostRef) empty() bool { return h.Provider == "" && h.Ref == "" }

// Machine is the kit's authoritative record for one unit of inventory. It
// carries both substrate truth (instance_type, zone, capacity, price,
// resources, host) supplied by the [Backend] and BigFleet bookkeeping
// (state, cluster binding, shard_metadata) owned by the kit. The store
// persists it verbatim, so it is what makes a provider restart lossless.
type Machine struct {
	ID    string
	State State
	Host  HostRef

	// Substrate truth — supplied by the Backend, validated on import,
	// never blanked by a lifecycle transition.
	InstanceType            string
	Zone                    string
	CapacityType            CapacityType
	PricePerHour            float64
	InterruptionProbability float64
	Resources               map[string]string
	Allocatable             map[string]string
	Labels                  map[string]string

	// BigFleet bookkeeping — owned by the kit.
	//
	// Cluster is the binding Configure established (copied from
	// ConfigureRequest.cluster_id). Populated while the binding exists
	// (Configuring, Configured, Draining); cleared when a Drain completes
	// back to Idle.
	Cluster string
	// ShardMetadata is the opaque map stored verbatim from
	// ConfigureRequest.shard_metadata, echoed byte-for-byte on every
	// snapshot, and cleared together with Cluster when a Drain completes.
	// Never interpreted.
	ShardMetadata map[string]string
	// LastError is populated when State == Failed.
	LastError string
}

// clone returns a deep copy so a snapshot handed to the store (or returned
// to a caller) cannot be mutated under the kit's feet, and vice versa.
func (m *Machine) clone() *Machine {
	if m == nil {
		return nil
	}
	c := *m
	c.Resources = cloneMap(m.Resources)
	c.Allocatable = cloneMap(m.Allocatable)
	c.Labels = cloneMap(m.Labels)
	c.ShardMetadata = cloneMap(m.ShardMetadata)
	return &c
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// kind enumerates the four mutating lifecycle operations. Idempotency is
// keyed on (machine_id, kind): Create and Drain both target the Idle state,
// so the operation kind — not the target state alone — is the
// discriminator (matching pkg/provider/fake).
type kind uint8

const (
	kindCreate kind = iota
	kindConfigure
	kindDrain
	kindDelete
)

func (k kind) String() string {
	switch k {
	case kindCreate:
		return "create"
	case kindConfigure:
		return "configure"
	case kindDrain:
		return "drain"
	case kindDelete:
		return "delete"
	default:
		return "unknown"
	}
}

// targets returns the (transitional, stable) state pair a kind drives the
// machine through.
func (k kind) targets() (transitional, stable State) {
	switch k {
	case kindCreate:
		return StateCreating, StateIdle
	case kindConfigure:
		return StateConfiguring, StateConfigured
	case kindDrain:
		return StateDraining, StateIdle
	case kindDelete:
		return StateDeleting, StateSpeculative
	default:
		return StateUnspecified, StateUnspecified
	}
}

// canStart reports whether a transition of the given kind may begin from
// the current state. It encodes the only four legal entry edges:
// Speculative→Creating, Idle→Configuring, Configured→Draining,
// Idle→Deleting. Every other request is an out-of-position transition and
// is rejected — never with FAILED_PRECONDITION, which is reserved for
// fencing.
func canStart(cur State, k kind) bool {
	switch k {
	case kindCreate:
		return cur == StateSpeculative
	case kindConfigure:
		return cur == StateIdle
	case kindDrain:
		return cur == StateConfigured
	case kindDelete:
		return cur == StateIdle
	default:
		return false
	}
}
