package main

import "testing"

// allocatable must reflect the OCPU→vCPU convention: x86 shapes expose 2 vCPU per
// OCPU, Ampere (A1) shapes 1 vCPU per OCPU.
func TestAllocatable_OCPUtoVCPU(t *testing.T) {
	x86 := allocatable("VM.Standard.E5.Flex", 4, 32)
	if x86["cpu"] != "8" {
		t.Errorf("x86 flex 4 OCPU cpu = %q, want 8 (2 vCPU/OCPU)", x86["cpu"])
	}
	if x86["memory"] != "32Gi" {
		t.Errorf("x86 flex memory = %q, want 32Gi", x86["memory"])
	}
	arm := allocatable("VM.Standard.A1.Flex", 4, 24)
	if arm["cpu"] != "4" {
		t.Errorf("Ampere flex 4 OCPU cpu = %q, want 4 (1 vCPU/OCPU)", arm["cpu"])
	}
}

// A GPU shape advertises its accelerators in allocatable.
func TestAllocatable_GPU(t *testing.T) {
	a := allocatable("VM.GPU.A10.1", 0, 0)
	if a["nvidia.com/gpu"] != "1" {
		t.Errorf("GPU shape allocatable nvidia.com/gpu = %q, want 1 (got %v)", a["nvidia.com/gpu"], a)
	}
	if a["cpu"] == "" || a["memory"] == "" {
		t.Errorf("GPU shape missing cpu/memory: %v", a)
	}
}

// An unknown shape yields nil (the kit then treats allocatable == resources and
// the FileStore restores the real values).
func TestAllocatable_UnknownShape(t *testing.T) {
	if a := allocatable("VM.Unknown.Shape", 2, 8); a != nil {
		t.Errorf("unknown shape allocatable = %v, want nil", a)
	}
}

// Non-whole-GiB memory renders as Mi without precision loss.
func TestFmtGiB(t *testing.T) {
	if got := fmtGiB(16); got != "16Gi" {
		t.Errorf("fmtGiB(16) = %q, want 16Gi", got)
	}
	if got := fmtGiB(1.5); got != "1536Mi" {
		t.Errorf("fmtGiB(1.5) = %q, want 1536Mi", got)
	}
}
