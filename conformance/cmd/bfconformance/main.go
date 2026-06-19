// Command bfconformance certifies a BigFleet capacity provider: it builds and
// boots the provider (credential-free), runs BOTH the immovable upstream
// conformance baseline AND this repo's extension suite against it, maps every
// result onto the frozen behavior registry, and emits a JUnit XML + JSON
// certification report with per-behavior coverage and a profile-aware verdict.
//
// Usage:
//
//	go -C conformance run ./cmd/bfconformance -provider aws -profile core,cloud -out /tmp/report
//	go -C conformance run ./cmd/bfconformance -target 127.0.0.1:9000   # already-running provider
//
// It absorbs hack/run-certify.sh (build/boot/resolve-bigfleet/run-both-suites)
// and adds machine-readable reporting + coverage accounting on top.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var (
		providerName = flag.String("provider", "", "provider under providers/ to certify (required unless -target)")
		target       = flag.String("target", "", "host:port of an already-running provider (skips build/boot)")
		profileCSV   = flag.String("profile", "core", "comma-separated profiles to certify against (core,cloud,bare-metal,spot,scale,durable,fault)")
		seed         = flag.Int("seed-count", 256, "Speculative seed count when booting the provider")
		port         = flag.Int("port", 9099, "port to boot the provider on")
		outDir       = flag.String("out", "", "directory for junit.xml + report.json (default: stdout summary only)")
		skipBaseline = flag.Bool("skip-baseline", false, "skip the upstream baseline lane (extension only)")
	)
	flag.Parse()

	repoRoot, err := findRepoRoot()
	if err != nil {
		fatal("%v", err)
	}
	profiles := splitCSV(*profileCSV)
	ts := time.Now().UTC().Format(time.RFC3339)

	// Resolve the endpoint: either an already-running --target, or build+boot.
	addr := *target
	label := *target
	if addr == "" {
		if *providerName == "" {
			fatal("set -provider <name> or -target host:port")
		}
		label = *providerName
		bin, err := buildProvider(repoRoot, *providerName)
		if err != nil {
			fatal("%v", err)
		}
		logPath := filepath.Join(os.TempDir(), "bfconformance-"+*providerName+".log")
		fmt.Fprintf(os.Stderr, ">> booting %s on 127.0.0.1:%d (seed=%d)\n", *providerName, *port, *seed)
		prov, err := boot(bin, *providerName, fmt.Sprintf("127.0.0.1:%d", *port), *seed, logPath)
		if err != nil {
			fatal("%v", err)
		}
		defer prov.stop()
		addr = prov.addr
	}

	// Lane 1: the immovable upstream baseline (run from the pinned bigfleet src).
	var base []testResult
	baselineRan := !*skipBaseline
	if baselineRan {
		src, err := bigfleetSrc(repoRoot)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Fprintf(os.Stderr, ">> [1/2] upstream baseline (%s)\n", addr)
		cmd := exec.Command("go", "test", "-json", "-tags=conformance", "-count=1",
			"-run", "^TestConformance_", "./test/conformance/...", "-target="+addr)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GOWORK=off")
		base, _ = runJSONTests(cmd)
	}

	// Lane 2: the extension suite.
	fmt.Fprintf(os.Stderr, ">> [2/2] extension suite (%s)\n", addr)
	extCmd := exec.Command("go", "-C", "conformance", "test", "-json", "-tags=certify", "-count=1",
		"./suite/...", "-target="+addr)
	extCmd.Dir = repoRoot
	extCmd.Env = append(os.Environ(), "GOWORK=off")
	ext, _ := runJSONTests(extCmd)

	// Lane 3 (fault): when the "fault" profile is selected, certify the B7xx
	// failure/timeout/recovery behaviors against the reference faultprovider,
	// which injects substrate faults on command. It boots on its OWN port (the
	// provider under test cannot inject faults), runs the fault-tagged suite, and
	// its results are MERGED into the extension results so the B7xx map onto the
	// registry like every other behavior.
	if contains(profiles, "fault") {
		faultRes, err := runFaultLane(repoRoot, *port+1)
		if err != nil {
			fatal("%v", err)
		}
		ext = append(ext, faultRes...)
	}

	// Lane 4 (durable): when the "durable" profile is selected, certify the
	// B10xx restart-recovery behaviors. This lane OWNS the provider lifecycle —
	// it boots the provider under test with a --state FileStore, drives durable
	// state over a raw client, KILLS the process, re-boots a fresh one against
	// the SAME --state, and asserts every piece of durable state survived. It
	// needs a real provider binary (a --target endpoint cannot be killed and
	// re-booted), so it requires -provider. Its results MERGE into ext so the
	// B10xx map onto the registry like every other behavior.
	if contains(profiles, "durable") {
		if *providerName == "" {
			fatal("the durable profile requires -provider <name> (the lane kills and re-boots the provider; a --target endpoint cannot be restarted)")
		}
		durRes, err := runDurableLane(repoRoot, *providerName, *port+2)
		if err != nil {
			fatal("%v", err)
		}
		ext = append(ext, durRes...)
	}

	rep := buildReport(label, profiles, ts, base, ext, baselineRan)
	printSummary(rep)
	if *outDir != "" {
		if err := writeReports(*outDir, rep, base, ext); err != nil {
			fatal("write reports: %v", err)
		}
		fmt.Fprintf(os.Stderr, ">> wrote %s/{report.json,junit.xml}\n", *outDir)
	}
	if rep.Verdict != "CERTIFIED" {
		os.Exit(1)
	}
}

// findRepoRoot walks up from the cwd to the directory that holds both
// providers/ and conformance/ (the mono-repo root).
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if isDir(filepath.Join(dir, "providers")) && isDir(filepath.Join(dir, "conformance")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find the bigfleet-providers root (a dir with providers/ and conformance/) above %s", mustGetwd())
		}
		dir = parent
	}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func mustGetwd() string {
	d, _ := os.Getwd()
	return d
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bfconformance: "+format+"\n", args...)
	os.Exit(2)
}
