package registry

import (
	"regexp"
	"testing"
)

var idRe = regexp.MustCompile(`^B[1-9][0-9]{2,3}$`)

// TestCatalogWellFormed pins the registry's invariants so it can't silently rot:
// ids are unique and well-formed, every field is sane, and capability/profile/
// phase combinations are coherent. The runner and suite both trust these.
func TestCatalogWellFormed(t *testing.T) {
	if len(Catalog) == 0 {
		t.Fatal("Catalog is empty")
	}
	validProfiles := map[string]bool{
		"core": true, "cloud": true, "bare-metal": true, "spot": true,
		"scale": true, "durable": true, "fault": true,
	}
	validCaps := map[string]bool{
		"": true, "Delete": true, "SinceRevision": true,
		"Durable": true, "Fault": true, "Scale": true,
	}
	seen := map[string]bool{}
	for _, b := range Catalog {
		if !idRe.MatchString(b.ID) {
			t.Errorf("%s: id is not well-formed (want B<area><nn>)", b.ID)
		}
		if seen[b.ID] {
			t.Errorf("%s: duplicate id", b.ID)
		}
		seen[b.ID] = true
		if b.Title == "" || b.Area == "" {
			t.Errorf("%s: empty Title or Area", b.ID)
		}
		if len(b.Profiles) == 0 {
			t.Errorf("%s: no profiles (every behavior belongs to >=1 profile)", b.ID)
		}
		for _, p := range b.Profiles {
			if !validProfiles[p] {
				t.Errorf("%s: unknown profile %q", b.ID, p)
			}
		}
		if !validCaps[b.Capability] {
			t.Errorf("%s: unknown capability %q", b.ID, b.Capability)
		}
		switch b.Phase {
		case 2, 4, 5:
		default:
			t.Errorf("%s: unexpected phase %d (want 2, 4, or 5)", b.ID, b.Phase)
		}
		// A non-black-box behavior needs runner orchestration / a configurable
		// backend, so it can only live in the fault/durability (4) or scale (5)
		// lanes — never the pure phase-2 black-box suites.
		if !b.BlackBox && b.Phase == 2 {
			t.Errorf("%s: BlackBox=false but Phase=2 (non-wire behaviors belong to phase 4/5)", b.ID)
		}
		// Capability gates must agree with their lane.
		if b.Capability == "Fault" && b.Phase != 4 {
			t.Errorf("%s: Fault capability outside the fault lane (phase %d)", b.ID, b.Phase)
		}
		if b.Capability == "Scale" && b.Phase != 5 {
			t.Errorf("%s: Scale capability outside the scale lane (phase %d)", b.ID, b.Phase)
		}
	}
}

// TestByID round-trips every catalog id.
func TestByID(t *testing.T) {
	for _, id := range IDs() {
		if b, ok := ByID(id); !ok || b.ID != id {
			t.Errorf("ByID(%q) failed to round-trip", id)
		}
	}
	if _, ok := ByID("B000-nope"); ok {
		t.Error("ByID returned ok for a nonexistent id")
	}
}
