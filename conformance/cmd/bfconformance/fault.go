package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// runFaultLane builds and boots the reference faultprovider (which injects
// substrate faults on command), runs the fault-tagged B7xx suite against it on
// its own port, and returns the testResults so the caller can merge them into
// the extension results before report-building. The faultprovider is always
// stopped before returning.
//
// It deliberately uses a SHORT --transition-timeout (2s) so the timeout-shaped
// behaviors (B703/B704) run fast, and --seed-count=64 so every B7xx test gets a
// fresh Speculative machine.
func runFaultLane(repoRoot string, port int) ([]testResult, error) {
	bin, err := buildFaultProvider(repoRoot)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	logPath := filepath.Join(os.TempDir(), "bfconformance-faultprovider.log")
	fmt.Fprintf(os.Stderr, ">> [fault] booting faultprovider on %s (seed=64, transition-timeout=2s)\n", addr)
	prov, err := bootFaultProvider(bin, addr, logPath)
	if err != nil {
		return nil, err
	}
	defer prov.stop()

	fmt.Fprintf(os.Stderr, ">> [fault] fault suite (%s)\n", addr)
	cmd := exec.Command("go", "-C", "conformance", "test", "-json", "-tags=certify,fault", "-count=1",
		"-run", "TestB7", "./suite/...", "-target="+addr)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	res, _ := runJSONTests(cmd)
	return res, nil
}

// buildFaultProvider compiles conformance/faultprovider to bin/faultprovider
// (GOWORK=off, the multi-module discipline).
func buildFaultProvider(repoRoot string) (string, error) {
	bin := filepath.Join(repoRoot, "bin", "faultprovider")
	if err := os.MkdirAll(filepath.Join(repoRoot, "bin"), 0o755); err != nil {
		return "", err
	}
	if err := run(repoRoot, []string{"GOWORK=off"}, "go", "-C", "conformance", "build", "-o", bin, "./faultprovider"); err != nil {
		return "", fmt.Errorf("build faultprovider: %w", err)
	}
	return bin, nil
}

// bootFaultProvider starts the faultprovider binary on addr with the fault-lane
// flags (short 2s transition-timeout, in-memory store) and waits until its gRPC
// port accepts a connection.
func bootFaultProvider(bin, addr, logPath string) (*provider, error) {
	return bootFaultProviderWith(bin, addr, logPath, "2s", "")
}

// bootFaultProviderWith starts the faultprovider binary on addr with an explicit
// --transition-timeout and an optional --state path (empty = in-memory only),
// waiting until its gRPC port accepts a connection. The durable lane's B1006
// cycle uses a GENEROUS timeout (so a CONFIGURING machine sits long enough to be
// killed mid-transition) and a --state FileStore (so the orphaned record
// survives the kill and recoverInterrupted moves it to FAILED on reload).
func bootFaultProviderWith(bin, addr, logPath, transitionTimeout, statePath string) (*provider, error) {
	args := []string{
		"--addr=" + addr,
		"--provider=fault",
		"--seed-count=64",
		"--transition-timeout=" + transitionTimeout,
	}
	if statePath != "" {
		args = append(args, "--state="+statePath)
	}
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("start faultprovider: %w", err)
	}
	_ = lf.Close()
	p := &provider{name: "faultprovider", addr: addr, cmd: cmd, logPath: logPath}
	if err := p.waitReady(20 * time.Second); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

// contains reports whether s is in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
