package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/pem"
	"fmt"
	"mime/multipart"
	"net"
	"net/textproto"
	"strings"

	"golang.org/x/crypto/ssh"
)

// metaHostKeyFP is the OpenStack server-metadata key that pins a server's SSH
// host-key fingerprint so Configure/Drain can verify the host they connect to.
// The value is base32(sha256(host pubkey)) — 52 lowercase [a-z2-7] chars.
const metaHostKeyFP = "bigfleet-host-key-fp"

// fpEncoding encodes binary into a metadata-safe value (lowercase base32 without
// padding → only [a-z2-7]).
var fpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// hostKeyMaterial is a generated ed25519 SSH host keypair for one server. The
// private key is injected into the server via cloud-init so the host boots with
// a key we already know; the fingerprint is pinned in server metadata and
// verified on every later SSH connection.
type hostKeyMaterial struct {
	privatePEM  string // OpenSSH PEM, injected via cloud-init ssh_keys.ed25519_private
	publicAuthz string // "ssh-ed25519 AAAA... bigfleet-host"
	fingerprint string // base32(sha256(pubkey wire bytes)) — pinned in metaHostKeyFP
}

// generateHostKey mints a fresh ed25519 host keypair.
func generateHostKey() (hostKeyMaterial, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return hostKeyMaterial{}, fmt.Errorf("generate ed25519 host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "bigfleet-host")
	if err != nil {
		return hostKeyMaterial{}, fmt.Errorf("marshal host private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return hostKeyMaterial{}, fmt.Errorf("marshal host public key: %w", err)
	}
	return hostKeyMaterial{
		privatePEM:  string(pem.EncodeToMemory(block)),
		publicAuthz: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))),
		fingerprint: hostKeyFingerprint(sshPub),
	}, nil
}

// hostKeyFingerprint is the stable, metadata-safe pin for an SSH host public
// key: base32(sha256(wire bytes)), lowercased. Deterministic across processes,
// so a fingerprint persisted in metadata round-trips a provider restart.
func hostKeyFingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return strings.ToLower(fpEncoding.EncodeToString(sum[:]))
}

// cloudConfig renders the cloud-init #cloud-config that installs this host key,
// so the server boots presenting the key we pinned.
func (hk hostKeyMaterial) cloudConfig() string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_keys:\n")
	b.WriteString("  ed25519_private: |\n")
	for _, line := range strings.Split(strings.TrimRight(hk.privatePEM, "\n"), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("  ed25519_public: ")
	b.WriteString(hk.publicAuthz)
	b.WriteString("\n")
	return b.String()
}

// hostKeyCallback verifies a presented host key against the pinned fingerprint.
// When expectedFP is non-empty it requires an exact match and rejects anything
// else (a possible MITM). When expectedFP is empty — a server we did not create
// (orphan) or one provisioned before host-key pinning — it trust-on-first-uses:
// it records the key via onTOFU and accepts, so all later connections are
// verified against it. The residual risk is confined to that first connection
// and is documented in security.md.
func hostKeyCallback(expectedFP string, onTOFU func(fp string)) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := hostKeyFingerprint(key)
		if expectedFP == "" {
			if onTOFU != nil {
				onTOFU(got)
			}
			return nil
		}
		if got != expectedFP {
			return fmt.Errorf("host key mismatch: pinned %q, presented %q (possible MITM — refusing)", expectedFP, got)
		}
		return nil
	}
}

// buildUserData assembles the cloud-init user-data delivered at server create:
// the operator's base user-data (if any) plus a cloud-config that installs the
// generated host key. With no base it returns the bare host-key cloud-config;
// with a base it wraps both in a MIME multipart archive cloud-init understands,
// so the host-key injection composes with whatever the operator supplied.
func buildUserData(base []byte, hostCfg string) (string, error) {
	if len(bytes.TrimSpace(base)) == 0 {
		return hostCfg, nil
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q\nMIME-Version: 1.0\n\n", mw.Boundary())
	addPart := func(ctype string, body []byte) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ctype)
		h.Set("MIME-Version", "1.0")
		pw, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = pw.Write(body)
		return err
	}
	if err := addPart(baseUserDataContentType(base), base); err != nil {
		return "", err
	}
	if err := addPart("text/cloud-config", []byte(hostCfg)); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	return header + buf.String(), nil
}

// baseUserDataContentType guesses the cloud-init MIME type of operator-supplied
// base user-data from its leading bytes, so a shell-script base is not misparsed
// as cloud-config.
func baseUserDataContentType(base []byte) string {
	s := strings.TrimLeft(string(base), " \t\r\n")
	switch {
	case strings.HasPrefix(s, "#cloud-config"):
		return "text/cloud-config"
	case strings.HasPrefix(s, "#!"):
		return "text/x-shellscript"
	case strings.HasPrefix(s, "#cloud-boothook"):
		return "text/cloud-boothook"
	default:
		return "text/cloud-config"
	}
}
