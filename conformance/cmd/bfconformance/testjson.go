package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// testEvent is one record from `go test -json` (the test2json stream).
type testEvent struct {
	Action  string  `json:"Action"` // run|output|pass|fail|skip|pause|cont|start
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// testResult is the finalized outcome of one test function.
type testResult struct {
	Pkg       string
	Name      string
	Outcome   string // pass|fail|skip
	Elapsed   float64
	Behaviors []string // registry leaf-ids the test bound itself to (BEHAVIOR markers)
	Output    string   // captured output (kept for failures/skips)
}

var behaviorMarkerRe = regexp.MustCompile(`BEHAVIOR (B[0-9]+)`)

// runJSONTests executes a `go test -json ...` command (suiteCmd already includes
// the binary + args), streams the test2json output, and returns one testResult
// per test function plus the raw exit error. It does not fail on a non-zero exit
// (a failing suite still yields parseable results); the caller inspects results.
func runJSONTests(cmd *exec.Cmd) ([]testResult, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	results := parseEvents(stdout)
	waitErr := cmd.Wait()
	return results, waitErr
}

// parseEvents folds a test2json event stream into per-test results.
func parseEvents(r io.Reader) []testResult {
	type acc struct {
		out strings.Builder
	}
	pending := map[string]*acc{} // pkg/test -> accumulating output
	var order []string
	final := map[string]*testResult{}

	key := func(e testEvent) string { return e.Package + "\x00" + e.Test }
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e testEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // non-JSON line (rare build noise); skip
		}
		if e.Test == "" {
			continue // package-level event
		}
		k := key(e)
		a := pending[k]
		if a == nil {
			a = &acc{}
			pending[k] = a
			order = append(order, k)
		}
		switch e.Action {
		case "output":
			a.out.WriteString(e.Output)
		case "pass", "fail", "skip":
			out := a.out.String()
			tr := &testResult{
				Pkg:       e.Package,
				Name:      e.Test,
				Outcome:   e.Action,
				Elapsed:   e.Elapsed,
				Behaviors: behaviorIDs(out),
				Output:    out,
			}
			final[k] = tr
		}
	}

	var results []testResult
	for _, k := range order {
		if tr, ok := final[k]; ok {
			// Skip parent results for table subtests (Test contains "/") only if
			// the parent itself produced an outcome — keep all leaf + top results;
			// behavior markers are emitted at the top-level test, which is enough.
			results = append(results, *tr)
		}
	}
	return results
}

func behaviorIDs(output string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, m := range behaviorMarkerRe.FindAllStringSubmatch(output, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	return ids
}

// firstLine returns the first non-empty line of s, trimmed (for compact failure
// summaries).
func firstFailLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "=== ") || strings.HasPrefix(ln, "--- ") || strings.HasPrefix(ln, "BEHAVIOR ") {
			continue
		}
		return ln
	}
	return ""
}
