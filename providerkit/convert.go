package providerkit

import (
	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// machineToProto renders a kit Machine as the wire message. It is the single
// place the field-shape contract is assembled: instance_type / zone /
// capacity_type are top-level (never labels-only); host is nil while the
// machine has no real host; cluster and shard_metadata ride along while a
// binding exists.
func machineToProto(m *Machine) *pb.Machine {
	out := &pb.Machine{
		Id:                      m.ID,
		State:                   stateToProto(m.State),
		InstanceType:            m.InstanceType,
		Zone:                    m.Zone,
		CapacityType:            capacityToProto(m.CapacityType),
		PricePerHour:            m.PricePerHour,
		InterruptionProbability: m.InterruptionProbability,
		Labels:                  cloneMap(m.Labels),
		Cluster:                 m.Cluster,
		ShardMetadata:           cloneMap(m.ShardMetadata),
		LastError:               m.LastError,
	}
	if !m.Host.empty() {
		out.Host = &pb.HostRef{Provider: m.Host.Provider, Ref: m.Host.Ref}
	}
	if len(m.Resources) > 0 {
		out.Resources = &pb.Resources{Resources: cloneMap(m.Resources)}
	}
	if len(m.Allocatable) > 0 {
		out.Allocatable = &pb.Resources{Resources: cloneMap(m.Allocatable)}
	}
	return out
}

func stateToProto(s State) pb.MachineState {
	switch s {
	case StateSpeculative:
		return pb.MachineState_MACHINE_STATE_SPECULATIVE
	case StateCreating:
		return pb.MachineState_MACHINE_STATE_CREATING
	case StateIdle:
		return pb.MachineState_MACHINE_STATE_IDLE
	case StateConfiguring:
		return pb.MachineState_MACHINE_STATE_CONFIGURING
	case StateConfigured:
		return pb.MachineState_MACHINE_STATE_CONFIGURED
	case StateDraining:
		return pb.MachineState_MACHINE_STATE_DRAINING
	case StateDeleting:
		return pb.MachineState_MACHINE_STATE_DELETING
	case StateFailed:
		return pb.MachineState_MACHINE_STATE_FAILED
	default:
		return pb.MachineState_MACHINE_STATE_UNSPECIFIED
	}
}

func stateFromProto(s pb.MachineState) State {
	switch s {
	case pb.MachineState_MACHINE_STATE_SPECULATIVE:
		return StateSpeculative
	case pb.MachineState_MACHINE_STATE_CREATING:
		return StateCreating
	case pb.MachineState_MACHINE_STATE_IDLE:
		return StateIdle
	case pb.MachineState_MACHINE_STATE_CONFIGURING:
		return StateConfiguring
	case pb.MachineState_MACHINE_STATE_CONFIGURED:
		return StateConfigured
	case pb.MachineState_MACHINE_STATE_DRAINING:
		return StateDraining
	case pb.MachineState_MACHINE_STATE_DELETING:
		return StateDeleting
	case pb.MachineState_MACHINE_STATE_FAILED:
		return StateFailed
	default:
		return StateUnspecified
	}
}

func capacityToProto(c CapacityType) pb.CapacityType {
	switch c {
	case CapacityBareMetal:
		return pb.CapacityType_CAPACITY_TYPE_BARE_METAL
	case CapacityReserved:
		return pb.CapacityType_CAPACITY_TYPE_RESERVED
	case CapacityOnDemand:
		return pb.CapacityType_CAPACITY_TYPE_ON_DEMAND
	case CapacitySpot:
		return pb.CapacityType_CAPACITY_TYPE_SPOT
	default:
		return pb.CapacityType_CAPACITY_TYPE_UNSPECIFIED
	}
}

// CapacityFromProto maps a wire CapacityType into the kit enum. Exported so
// a Backend translating its own substrate catalogue can reuse it.
func CapacityFromProto(c pb.CapacityType) CapacityType {
	switch c {
	case pb.CapacityType_CAPACITY_TYPE_BARE_METAL:
		return CapacityBareMetal
	case pb.CapacityType_CAPACITY_TYPE_RESERVED:
		return CapacityReserved
	case pb.CapacityType_CAPACITY_TYPE_ON_DEMAND:
		return CapacityOnDemand
	case pb.CapacityType_CAPACITY_TYPE_SPOT:
		return CapacitySpot
	default:
		return CapacityUnspecified
	}
}
