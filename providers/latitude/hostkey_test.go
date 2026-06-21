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

// The deploy hostname must be DNS-safe, ≤63 chars, deterministic, and — the
// crucial property — COLLISION-FREE across distinct machine ids. The three slot
// ids below are the ones the round-2 review proved collided under the old
// truncating encoding (…/000, …/001, …/017); they must now map to distinct
// hostnames so adoption can never deliver one machine's join secret to another.
func TestDeployHostname_CollisionFree(t *testing.T) {
	ids := []string{
		"latitude-ash/on_demand/c2-small-x86/ASH/000",
		"latitude-ash/on_demand/c2-small-x86/ASH/001",
		"latitude-ash/on_demand/c2-small-x86/ASH/017",
		"latitude-ash/on_demand/c3-large-x86/NYC/000",
		"m1", "x",
	}
	seen := map[string]string{}
	for _, id := range ids {
		h := deployHostname(id)
		if !strings.HasPrefix(h, hostnamePrefix) {
			t.Errorf("hostname %q missing prefix", h)
		}
		if len(h) > 63 {
			t.Errorf("hostname %q exceeds 63 chars (%d)", h, len(h))
		}
		// DNS-safe: lowercase [a-z2-7] plus the prefix's hyphen.
		for _, c := range strings.TrimPrefix(h, hostnamePrefix) {
			if !((c >= 'a' && c <= 'z') || (c >= '2' && c <= '7')) {
				t.Errorf("hostname %q has non-DNS-safe char %q", h, c)
			}
		}
		if other, dup := seen[h]; dup {
			t.Fatalf("hostname collision: %q and %q both map to %q", other, id, h)
		}
		seen[h] = id
		// Deterministic: same id -> same hostname.
		if h2 := deployHostname(id); h2 != h {
			t.Errorf("deployHostname not deterministic for %q: %q vs %q", id, h, h2)
		}
	}
}
