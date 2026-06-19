package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/registry"
)

// laneSummary aggregates one test lane (baseline or extension).
type laneSummary struct {
	Tests   int `json:"tests"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

func summarize(rs []testResult) laneSummary {
	var s laneSummary
	for _, r := range rs {
		s.Tests++
		switch r.Outcome {
		case "pass":
			s.Passed++
		case "fail":
			s.Failed++
		case "skip":
			s.Skipped++
		}
	}
	return s
}

// behaviorResult is the per-behavior certification outcome.
type behaviorResult struct {
	ID       string   `json:"id"`
	Area     string   `json:"area"`
	Title    string   `json:"title"`
	Profiles []string `json:"profiles"`
	Phase    int      `json:"phase"`
	Required bool     `json:"required"` // required by the claimed profile(s)
	Status   string   `json:"status"`   // passed|failed|skipped|not_implemented
	Tests    []string `json:"tests,omitempty"`
}

type summary struct {
	TotalBehaviors  int `json:"total_behaviors"`
	Passed          int `json:"passed"`
	Failed          int `json:"failed"`
	Skipped         int `json:"skipped"`
	NotImplemented  int `json:"not_implemented"`
	RequiredFailed  int `json:"required_failed"`
	RequiredMissing int `json:"required_missing"`
}

// report is the full machine-readable certification report.
type report struct {
	Provider  string           `json:"provider"`
	Profiles  []string         `json:"profiles"`
	Timestamp string           `json:"timestamp"`
	Baseline  laneSummary      `json:"baseline"`
	Extension laneSummary      `json:"extension"`
	Behaviors []behaviorResult `json:"behaviors"`
	Summary   summary          `json:"summary"`
	Verdict   string           `json:"verdict"` // CERTIFIED|FAILED|INCOMPLETE
}

// buildReport maps test results onto the frozen registry and computes the
// certification verdict for the claimed profiles.
func buildReport(providerName string, profiles []string, ts string, base, ext []testResult, baselineRan bool) report {
	// Aggregate each behavior's status from the tests that emitted its marker.
	// failed dominates passed, passed dominates skipped.
	status := map[string]string{}
	tests := map[string][]string{}
	for _, r := range ext {
		for _, id := range r.Behaviors {
			tests[id] = append(tests[id], r.Name)
			switch r.Outcome {
			case "fail":
				status[id] = "failed"
			case "pass":
				if status[id] != "failed" {
					status[id] = "passed"
				}
			case "skip":
				if status[id] == "" {
					status[id] = "skipped"
				}
			}
		}
	}

	selected := map[string]bool{}
	for _, p := range profiles {
		selected[p] = true
	}

	rep := report{
		Provider:  providerName,
		Profiles:  profiles,
		Timestamp: ts,
		Baseline:  summarize(base),
		Extension: summarize(ext),
		Verdict:   "CERTIFIED",
	}
	for _, b := range registry.Catalog {
		st := status[b.ID]
		if st == "" {
			st = "not_implemented"
		}
		required := false
		for _, p := range b.Profiles {
			if selected[p] {
				required = true
				break
			}
		}
		rep.Behaviors = append(rep.Behaviors, behaviorResult{
			ID: b.ID, Area: b.Area, Title: b.Title, Profiles: b.Profiles,
			Phase: b.Phase, Required: required, Status: st, Tests: tests[b.ID],
		})
		rep.Summary.TotalBehaviors++
		switch st {
		case "passed":
			rep.Summary.Passed++
		case "failed":
			rep.Summary.Failed++
			if required {
				rep.Summary.RequiredFailed++
			}
		case "skipped":
			rep.Summary.Skipped++
		case "not_implemented":
			rep.Summary.NotImplemented++
			if required {
				rep.Summary.RequiredMissing++
			}
		}
	}

	// Verdict: a failed required behavior or any baseline failure => FAILED.
	// A required behavior with no implementing test => INCOMPLETE. Else CERTIFIED.
	// (A skipped behavior is skip-as-pass: the provider declares it inapplicable.)
	switch {
	case rep.Summary.RequiredFailed > 0 || (baselineRan && rep.Baseline.Failed > 0):
		rep.Verdict = "FAILED"
	case rep.Summary.RequiredMissing > 0:
		rep.Verdict = "INCOMPLETE"
	default:
		rep.Verdict = "CERTIFIED"
	}
	return rep
}

// writeReports writes report.json + junit.xml to dir.
func writeReports(dir string, rep report, base, ext []testResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	j, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(j, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "badge.json"), badgeJSON(rep), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "junit.xml"), junitXML(rep.Provider, base, ext), 0o644)
}

// printSummary writes a human-readable certification summary to stdout.
func printSummary(rep report) {
	fmt.Printf("\nBigFleet conformance — provider=%s profiles=%v\n", rep.Provider, rep.Profiles)
	fmt.Printf("  baseline : %d tests, %d passed, %d failed, %d skipped\n", rep.Baseline.Tests, rep.Baseline.Passed, rep.Baseline.Failed, rep.Baseline.Skipped)
	fmt.Printf("  extension: %d tests, %d passed, %d failed, %d skipped\n", rep.Extension.Tests, rep.Extension.Passed, rep.Extension.Failed, rep.Extension.Skipped)
	fmt.Printf("  behaviors: %d total — %d passed, %d failed, %d skipped, %d not-implemented\n",
		rep.Summary.TotalBehaviors, rep.Summary.Passed, rep.Summary.Failed, rep.Summary.Skipped, rep.Summary.NotImplemented)
	// Per-area coverage line.
	areas := map[string][2]int{} // area -> [passed, total-implemented(phase-applicable)]
	var areaOrder []string
	for _, b := range rep.Behaviors {
		c := areas[b.Area]
		if _, seen := areas[b.Area]; !seen {
			areaOrder = append(areaOrder, b.Area)
		}
		if b.Status == "passed" {
			c[0]++
		}
		c[1]++
		areas[b.Area] = c
	}
	sort.Strings(areaOrder)
	for _, a := range areaOrder {
		c := areas[a]
		fmt.Printf("    %-32s %d/%d passed\n", a, c[0], c[1])
	}
	if rep.Summary.RequiredFailed > 0 {
		fmt.Printf("  ✗ %d REQUIRED behavior(s) FAILED\n", rep.Summary.RequiredFailed)
	}
	if rep.Summary.RequiredMissing > 0 {
		fmt.Printf("  ! %d REQUIRED behavior(s) not yet implemented\n", rep.Summary.RequiredMissing)
	}
	fmt.Printf("\n>> VERDICT: %s\n", rep.Verdict)
}
