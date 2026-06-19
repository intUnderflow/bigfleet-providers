package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// scaleParams resolves the scale seed + soak duration, overridable via
// BFCONF_SCALE_SEED / BFCONF_SOAK so CI can run a lighter, faster scale lane
// (and a nightly job can crank it well above these defaults).
func scaleParams() (seed int, soak string) {
	seed, soak = 8000, "10s"
	if v := os.Getenv("BFCONF_SCALE_SEED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			seed = n
		}
	}
	if v := os.Getenv("BFCONF_SOAK"); v != "" {
		soak = v
	}
	return seed, soak
}

// runScaleLane certifies the B11xx SCALE & SOAK behaviors. It builds and boots
// the provider under test with a LARGE seeded inventory on its own port (so it
// never collides with the extension/fault/durable lanes), runs the scale-tagged
// suite against it, and returns the testResults so the caller can merge them
// into the extension results before report-building. The provider is always
// stopped before returning.
//
// Like the black-box extension lane it is a pure wire test against a long-lived
// provider (it does NOT kill/re-boot), but it OWNS the provider lifecycle so it
// can pick the large seed the scale assertions need. The seed and soak duration
// are cranked here (well above the suite's credential-free defaults) so the
// registry's "tens of thousands of machines" / "multi-minute soak" titles are
// exercised with real weight while staying fast enough for CI.
func runScaleLane(repoRoot, providerName string, port int) ([]testResult, error) {
	scaleSeed, soak := scaleParams()

	bin, err := buildProvider(repoRoot, providerName)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	logPath := filepath.Join(os.TempDir(), "bfconformance-"+providerName+"-scale.log")
	fmt.Fprintf(os.Stderr, ">> [scale] booting %s on %s (seed=%d)\n", providerName, addr, scaleSeed)
	prov, err := boot(bin, providerName, addr, scaleSeed, logPath)
	if err != nil {
		return nil, err
	}
	defer prov.stop()

	fmt.Fprintf(os.Stderr, ">> [scale] scale & soak suite (%s, scale-seed=%d, soak=%s)\n", addr, scaleSeed, soak)
	cmd := exec.Command("go", "-C", "conformance", "test", "-json", "-tags=certify,scale", "-count=1",
		"-run", "TestB11", "./suite/...",
		"-target="+addr,
		fmt.Sprintf("-scale-seed=%d", scaleSeed),
		"-soak="+soak,
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	res, _ := runJSONTests(cmd)
	return res, nil
}
