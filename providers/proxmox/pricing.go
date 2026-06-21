package main

// pricing supplies Machine.price_per_hour (USD/hour). Proxmox has no cloud bill,
// so there is no pricing API to read. The price is a synthetic, deterministic
// rate derived from the instance type's hardware (a per-vCPU + per-GiB rate), so
// cost-based scheduling demos are meaningful and a larger flavor costs
// proportionally more.
//
// An operator can pin an explicit per-type price via --prices instead; this is
// the default when none is given. The value is a relative ranking signal for the
// shard's effective-cost formula, not a real invoice, so approximate synthetic
// pricing is fine.
type pricing struct {
	catalog     *instanceCatalog
	perVCPUHour float64
	perGiBHour  float64
	overrides   map[string]float64 // instance_type -> explicit USD/hour
}

// Default synthetic rates: roughly cloud-comparable so the numbers look sane in
// a demo (a 2 vCPU / 4 GiB box lands near a cent/hour).
const (
	defaultPerVCPUHour = 0.0030
	defaultPerGiBHour  = 0.0008
)

func newPricing(catalog *instanceCatalog, perVCPUHour, perGiBHour float64, overrides map[string]float64) *pricing {
	if perVCPUHour <= 0 {
		perVCPUHour = defaultPerVCPUHour
	}
	if perGiBHour <= 0 {
		perGiBHour = defaultPerGiBHour
	}
	return &pricing{
		catalog:     catalog,
		perVCPUHour: perVCPUHour,
		perGiBHour:  perGiBHour,
		overrides:   overrides,
	}
}

// price returns USD/hour for a machine of the given instance type. Reads only
// static state (never blocks), so it is safe on the List/seed path.
func (p *pricing) price(instanceType string) float64 {
	if v, ok := p.overrides[instanceType]; ok {
		return v
	}
	cap, ok := p.catalog.capacity(instanceType)
	if !ok {
		return 0
	}
	gib := float64(cap.MemMiB) / 1024.0
	return float64(cap.VCPU)*p.perVCPUHour + gib*p.perGiBHour
}
