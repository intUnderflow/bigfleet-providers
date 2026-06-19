package main

import (
	"fmt"
	"strconv"
)

// instanceCapacity is the real hardware capacity of an EC2 instance type,
// used to populate Machine.allocatable (ADR-0022: the aggregate the engine's
// deficit math compares against demand; density = floor(allocatable /
// resources)).
type instanceCapacity struct {
	VCPU   int
	MemGiB int
}

// instanceTypeTable is a pinned snapshot of common instance types. It is
// intentionally small; a production deployment should expand it or source it
// live from ec2:DescribeInstanceTypes (cached) so every offered type resolves.
// A type missing here yields a nil allocatable, which the engine treats as
// "allocatable == resources" — wrong for real hardware, so keep it complete
// for the types you actually offer.
var instanceTypeTable = map[string]instanceCapacity{
	// General purpose (m6i / m7g).
	"m6i.large":   {2, 8},
	"m6i.xlarge":  {4, 16},
	"m6i.2xlarge": {8, 32},
	"m6i.4xlarge": {16, 64},
	"m6i.8xlarge": {32, 128},
	"m7g.large":   {2, 8},
	"m7g.xlarge":  {4, 16},
	"m7g.2xlarge": {8, 32},
	"m7g.4xlarge": {16, 64},
	// Compute optimised (c6i / c7g).
	"c6i.large":   {2, 4},
	"c6i.xlarge":  {4, 8},
	"c6i.2xlarge": {8, 16},
	"c6i.4xlarge": {16, 32},
	"c7g.large":   {2, 4},
	"c7g.xlarge":  {4, 8},
	"c7g.2xlarge": {8, 16},
	"c7g.4xlarge": {16, 32},
	// Memory optimised (r6i).
	"r6i.large":   {2, 16},
	"r6i.xlarge":  {4, 32},
	"r6i.2xlarge": {8, 64},
	"r6i.4xlarge": {16, 128},
	// GPU (g5) — accelerator types carry a label too (see offering.go).
	"g5.xlarge":   {4, 16},
	"g5.2xlarge":  {8, 32},
	"g5.4xlarge":  {16, 64},
	"g5.12xlarge": {48, 192},
}

// allocatable renders an instance type's real capacity as a Kubernetes-style
// resource map (deterministic strings). Returns nil for an unknown type.
func allocatable(instanceType string) map[string]string {
	c, ok := instanceTypeTable[instanceType]
	if !ok {
		return nil
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": fmt.Sprintf("%dGi", c.MemGiB),
	}
}
