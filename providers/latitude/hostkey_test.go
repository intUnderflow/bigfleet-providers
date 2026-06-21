package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testPubKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	return k
}

func TestGenerateHostKey_Shape(t *testing.T) {
	hk, err := generateHostKey()
	if err != nil {
		t.Fatalf("generateHostKey: %v", err)
	}
	if !strings.Contains(hk.privatePEM, "OPENSSH PRIVATE KEY") {
		t.Errorf("private PEM not OpenSSH format: %q", hk.privatePEM)
	}
	if !strings.HasPrefix(hk.publicAuthz, "ssh-ed25519 ") {
		t.Errorf("public authz not ed25519: %q", hk.publicAuthz)
	}
	if len(hk.fingerprint) == 0 {
		t.Error("empty fingerprint")
	}
	// The fingerprint must be a tag-safe value (lowercase base32: [a-z2-7]).
	for _, c := range hk.fingerprint {
		isLower := c >= 'a' && c <= 'z'
		isBase32Digit := c >= '2' && c <= '7'
		if !isLower && !isBase32Digit {
			t.Errorf("fingerprint has non-tag-safe char %q in %q", c, hk.fingerprint)
		}
	}
	// The rendered cloud-config must carry the key for cloud-init.
	cc := hk.cloudConfig()
	if !strings.HasPrefix(cc, "#cloud-config\n") || !strings.Contains(cc, "ed25519_private:") || !strings.Contains(cc, "ed25519_public:") {
		t.Errorf("cloud-config missing host-key directives:\n%s", cc)
	}
}

func TestHostKeyCallback_Verify(t *testing.T) {
	key := testPubKey(t)
	fp := hostKeyFingerprint(key)

	// Matching pin → accept.
	if err := hostKeyCallback(fp, nil)("h", nil, key); err != nil {
		t.Errorf("matching pin rejected: %v", err)
	}
	// Wrong pin → reject (possible MITM).
	if err := hostKeyCallback("differentfingerprint", nil)("h", nil, key); err == nil {
		t.Error("mismatched pin accepted; want rejection")
	}
	// No pin → TOFU: accept and surface the observed fingerprint.
	var recorded string
	if err := hostKeyCallback("", func(f string) { recorded = f })("h", nil, key); err != nil {
		t.Errorf("TOFU connect rejected: %v", err)
	}
	if recorded != fp {
		t.Errorf("TOFU recorded %q, want %q", recorded, fp)
	}
}

func TestBuildUserData(t *testing.T) {
	hk, err := generateHostKey()
	if err != nil {
		t.Fatalf("generateHostKey: %v", err)
	}
	hostCfg := hk.cloudConfig()

	// No base → bare host-key cloud-config.
	out, err := buildUserData(nil, hostCfg)
	if err != nil {
		t.Fatalf("buildUserData(nil): %v", err)
	}
	if out != hostCfg {
		t.Errorf("empty base should yield the bare host-key cloud-config")
	}

	// With a base → multipart MIME carrying both the base and the host key.
	base := []byte("#cloud-config\nruncmd:\n  - echo hi\n")
	out, err = buildUserData(base, hostCfg)
	if err != nil {
		t.Fatalf("buildUserData(base): %v", err)
	}
	if !strings.HasPrefix(out, "Content-Type: multipart/mixed;") {
		t.Errorf("multipart user-data missing MIME header:\n%s", out)
	}
	if !strings.Contains(out, "echo hi") || !strings.Contains(out, "ed25519_private:") {
		t.Errorf("multipart user-data dropped a part:\n%s", out)
	}
}

// The machine id must round-trip through the hostname carrier, so DescribeManaged
// can recover inventory from a deployed server's hostname.
func TestDeployHostname_RoundTrip(t *testing.T) {
	for _, id := range []string{"latitude-ash/on_demand/c2-small-x86/ASH/000", "m1", "x"} {
		h := deployHostname(id)
		if !strings.HasPrefix(h, hostnamePrefix) {
			t.Errorf("hostname %q missing prefix", h)
		}
		if len(h) > 63 {
			t.Errorf("hostname %q exceeds 63 chars", h)
		}
		got := decodeHostname(h)
		// Only ids short enough to fit round-trip exactly; longer ones truncate and
		// rely on the FileStore (documented).
		if len(hostnamePrefix+encodeMachineID(id)) <= 63 && got != id {
			t.Errorf("decodeHostname(%q) = %q, want %q", h, got, id)
		}
	}
	// A non-BigFleet hostname decodes to empty (not ours).
	if got := decodeHostname("some-random-host"); got != "" {
		t.Errorf("decodeHostname(non-bigfleet) = %q, want empty", got)
	}
}
