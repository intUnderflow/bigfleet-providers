package main

import (
	"fmt"
	"strconv"
)

// shapeSpec is the hardware shape of an OCI compute shape, used to populate
// Machine.allocatable — the per-machine hardware capacity the kubelet will
// advertise (ADR-0022), distinct from Machine.resources (the per-replica request
// shape the offering serves).
//
// OCI sells compute by OCPU (an Oracle CPU = one physical core). On x86 shapes
// one OCPU exposes two vCPU threads (hyperthreading), so the kubelet advertises
// allocatable cpu = ocpu × 2. Ampere (Arm) A1/A2 cores are single-threaded, so
// cpu = ocpu × 1. ThreadsPerOCPU records that convention per shape; it is the
// single place the OCPU→vCPU mapping is defined, and the docs cite it.
type shapeSpec struct {
	// Flex marks a flexible shape, whose OCPU/memory are chosen at launch
	// (from the offering) rather than fixed by the shape name. For a flex shape
	// OCPU/MemGiB below are unused.
	Flex           bool
	OCPU           float64 // fixed shapes only
	MemGiB         float64 // fixed shapes only
	ThreadsPerOCPU int     // vCPU exposed per OCPU (2 x86, 1 Ampere)
	GPUCount       int     // accelerators on the shape
	GPUResource    string  // Kubernetes extended-resource name, e.g. "nvidia.com/gpu"
}

func (s shapeSpec) threads() int {
	if s.ThreadsPerOCPU <= 0 {
		return 2
	}
	return s.ThreadsPerOCPU
}

// shapeTable is a pinned snapshot of common OCI compute shapes. Flexible shapes
// record only the OCPU→vCPU convention and any accelerators; fixed shapes record
// their full OCPU/RAM. The resolver reads only this table (OCI shape specs are
// either operator-chosen at launch, for flex, or immutable, for fixed), so it is
// deterministic and needs no live API call — the fake backend and credential-free
// certification produce correct allocatable offline.
//
// Sourced from the OCI Compute shapes catalogue. Standard.E5/E4 (AMD), Standard3
// (Intel), A1/A2 (Ampere Arm64), GPU (NVIDIA).
var shapeTable = map[string]shapeSpec{
	// Flexible x86 (AMD / Intel): 2 vCPU per OCPU.
	"VM.Standard.E5.Flex": {Flex: true, ThreadsPerOCPU: 2},
	"VM.Standard.E4.Flex": {Flex: true, ThreadsPerOCPU: 2},
	"VM.Standard3.Flex":   {Flex: true, ThreadsPerOCPU: 2},
	"VM.Optimized3.Flex":  {Flex: true, ThreadsPerOCPU: 2},
	// Flexible Ampere Arm64: 1 vCPU per OCPU (single-threaded cores).
	"VM.Standard.A1.Flex": {Flex: true, ThreadsPerOCPU: 1},
	"VM.Standard.A2.Flex": {Flex: true, ThreadsPerOCPU: 1},
	// Fixed GPU VM shapes (OCPU/RAM pinned by the shape).
	"VM.GPU.A10.1": {OCPU: 15, MemGiB: 240, ThreadsPerOCPU: 2, GPUCount: 1, GPUResource: "nvidia.com/gpu"},
	"VM.GPU.A10.2": {OCPU: 30, MemGiB: 480, ThreadsPerOCPU: 2, GPUCount: 2, GPUResource: "nvidia.com/gpu"},
	// Fixed bare-metal shapes (price_per_hour=0; capacity_type BARE_METAL).
	"BM.Standard.E5.192": {OCPU: 192, MemGiB: 2304, ThreadsPerOCPU: 2},
	"BM.Standard3.64":    {OCPU: 64, MemGiB: 1024, ThreadsPerOCPU: 2},
	"BM.Standard.A1.160": {OCPU: 160, MemGiB: 1024, ThreadsPerOCPU: 1},
	"BM.GPU.A100-v2.8":   {OCPU: 128, MemGiB: 2048, ThreadsPerOCPU: 2, GPUCount: 8, GPUResource: "nvidia.com/gpu"},
}

// allocatable renders the per-machine hardware capacity of a shape as a
// Kubernetes-style resource map. For a flexible shape it uses the offering's
// declared OCPUs/MemoryGB; for a fixed shape it uses the pinned spec. Returns nil
// for an unknown shape (the kit then treats allocatable == resources, so the
// FileStore — the primary restart path — restores the real values). Read-only and
// non-blocking: safe on the List/seed hot path.
func allocatable(shape string, ocpus, memGiB float64) map[string]string {
	spec, ok := shapeTable[shape]
	if !ok {
		return nil
	}
	if !spec.Flex {
		ocpus = spec.OCPU
		memGiB = spec.MemGiB
	}
	if ocpus <= 0 || memGiB <= 0 {
		return nil
	}
	vcpu := int(ocpus * float64(spec.threads()))
	out := map[string]string{
		"cpu":    strconv.Itoa(vcpu),
		"memory": fmtGiB(memGiB),
	}
	if spec.GPUCount > 0 && spec.GPUResource != "" {
		out[spec.GPUResource] = strconv.Itoa(spec.GPUCount)
	}
	return out
}

// fmtGiB renders a GiB quantity as a Kubernetes memory string, preferring a whole
// "<n>Gi" and falling back to "<n>Mi" when it is not a whole GiB, so the value
// round-trips without precision loss.
func fmtGiB(memGiB float64) string {
	mib := int64(memGiB * 1024)
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGi", mib/1024)
	}
	return fmt.Sprintf("%dMi", mib)
}
