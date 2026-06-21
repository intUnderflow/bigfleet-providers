package main

// DigitalOcean interruption model.
//
// DigitalOcean Droplets are an ON-DEMAND-only product: there is no spot /
// preemptible Droplet tier, and DigitalOcean does not reclaim a running Droplet
// for capacity reasons. So the genuine, provider-declared hourly interruption
// probability is exactly zero, and every machine this provider reports is
// CAPACITY_TYPE_ON_DEMAND.
//
// This is a deliberate, honest constant — not a placeholder. Because it is 0,
// the provider does NOT claim the `spot` conformance profile (a SPOT machine is
// required to declare a real, >0 interruption probability; DigitalOcean has no
// such machine to declare one for). If DigitalOcean ever ships a preemptible
// product, that is a future provider change, not a speculative field today.
const dropletInterruptionProbability = 0.0
