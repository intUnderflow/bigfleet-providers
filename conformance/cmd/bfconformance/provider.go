package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// run executes a command, streaming its stderr through, and returns its error.
func run(dir string, extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// output runs a command and returns its stdout.
func output(dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return out.String(), err
}

var pseudoVersionRe = regexp.MustCompile(`-[0-9]{14}-([0-9a-f]{12})$`)

// bigfleetSrc resolves a checkout of the pinned bigfleet version, which carries
// the immovable upstream conformance baseline. It reuses $BIGFLEET_SRC when it
// points at a tree with test/conformance, else clones the pinned pseudo-version
// commit into .cache/bigfleet-src — mirroring hack/run-certify.sh exactly so the
// runner and the script resolve identically.
func bigfleetSrc(repoRoot string) (string, error) {
	if s := os.Getenv("BIGFLEET_SRC"); s != "" {
		if _, err := os.Stat(filepath.Join(s, "test", "conformance")); err == nil {
			return s, nil
		}
	}
	src := filepath.Join(repoRoot, ".cache", "bigfleet-src")
	ver, err := output(repoRoot, []string{"GOWORK=off"}, "go", "list", "-m", "-f", "{{.Version}}", "github.com/intUnderflow/bigfleet")
	if err != nil {
		return "", fmt.Errorf("resolve bigfleet version: %w", err)
	}
	ver = strings.TrimSpace(ver)
	ref := ver
	if m := pseudoVersionRe.FindStringSubmatch(ver); m != nil {
		ref = m[1] // the 12-hex commit from a vX.Y.Z-<ts>-<commit> pseudo-version
	}
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		_ = os.RemoveAll(src)
		fmt.Fprintf(os.Stderr, ">> cloning bigfleet into %s\n", src)
		if err := run(repoRoot, nil, "git", "clone", "--filter=blob:none", "--quiet",
			"https://github.com/intUnderflow/bigfleet.git", src); err != nil {
			return "", fmt.Errorf("clone bigfleet: %w", err)
		}
	}
	if err := run(repoRoot, nil, "git", "-C", src, "checkout", "--quiet", ref); err != nil {
		return "", fmt.Errorf("checkout bigfleet %s: %w", ref, err)
	}
	return src, nil
}

// provider is a built + booted provider process under certification.
type provider struct {
	name    string
	addr    string
	cmd     *exec.Cmd
	logPath string
}

// buildProvider compiles providers/<name> to bin/<name> (GOWORK=off, the
// multi-module discipline).
func buildProvider(repoRoot, name string) (string, error) {
	bin := filepath.Join(repoRoot, "bin", name)
	if err := os.MkdirAll(filepath.Join(repoRoot, "bin"), 0o755); err != nil {
		return "", err
	}
	if err := run(repoRoot, []string{"GOWORK=off"}, "go", "-C", filepath.Join("providers", name), "build", "-o", bin, "."); err != nil {
		return "", fmt.Errorf("build provider %s: %w", name, err)
	}
	return bin, nil
}

// boot starts the provider binary on addr, seeded for an extension run, with any
// extra flags (e.g. --state=PATH for the durability lane). It returns once the
// gRPC port accepts a connection.
func boot(bin, name, addr string, seed int, logPath string, extra ...string) (*provider, error) {
	// --use-fake-backend: certification is credential-free, so the provider must be
	// told to run its in-memory fake explicitly (providers fail closed on a silent
	// fake — a misconfigured real deployment must not come up on a simulation).
	args := append([]string{"--addr=" + addr, "--provider=certify", "--use-fake-backend", fmt.Sprintf("--seed-count=%d", seed)}, extra...)
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("start provider: %w", err)
	}
	_ = lf.Close() // the child holds its own fd
	p := &provider{name: name, addr: addr, cmd: cmd, logPath: logPath}
	if err := p.waitReady(20 * time.Second); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

func (p *provider) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", p.addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("provider %s did not listen on %s within %s (see %s)", p.name, p.addr, timeout, p.logPath)
}

// stop kills the provider and reaps it.
func (p *provider) stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
}
