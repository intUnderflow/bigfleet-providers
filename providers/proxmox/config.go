package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// instanceTypeSpec is the JSON shape of one catalog entry in an --instance-types
// file: a flavor name mapping to its hardware and source template.
type instanceTypeSpec struct {
	VCPU         int   `json:"vcpu"`
	MemoryMiB    int64 `json:"memory_mib"`
	TemplateVMID int   `json:"template_vmid"`
}

// loadInstanceTypes reads an instance-type catalog from a JSON file (an object
// of name -> {vcpu, memory_mib, template_vmid}). Empty path returns nil (the
// built-in default is used). A zero template_vmid falls back to the default
// template configured on the catalog.
func loadInstanceTypes(path string) (map[string]vmCapacity, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read instance-types %s: %w", path, err)
	}
	var raw map[string]instanceTypeSpec
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse instance-types %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("instance-types %s is empty", path)
	}
	out := make(map[string]vmCapacity, len(raw))
	for name, spec := range raw {
		if spec.VCPU <= 0 || spec.MemoryMiB <= 0 {
			return nil, fmt.Errorf("instance-types %s: %q must have positive vcpu and memory_mib", path, name)
		}
		out[name] = vmCapacity{VCPU: spec.VCPU, MemMiB: spec.MemoryMiB, TemplateVMID: spec.TemplateVMID}
	}
	return out, nil
}

// parseNodes parses the --nodes flag (a comma-separated list of Proxmox node
// names, each a BigFleet zone) into a deduplicated, ordered slice. Empty returns
// nil (the fake backend then synthesises two zones).
func parseNodes(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		node := strings.TrimSpace(part)
		if node == "" {
			continue
		}
		if strings.Contains(node, "/") {
			return nil, fmt.Errorf("--nodes entry %q must not contain '/'", node)
		}
		if seen[node] {
			return nil, fmt.Errorf("--nodes lists %q twice", node)
		}
		seen[node] = true
		out = append(out, node)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--nodes %q parsed to no nodes", s)
	}
	return out, nil
}

// parsePriceOverrides parses a --prices flag of "type=usd" pairs into a map.
func parsePriceOverrides(s string) (map[string]float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[string]float64{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			return nil, fmt.Errorf("--prices entry %q must be type=usd", part)
		}
		name := strings.TrimSpace(part[:eq])
		v, err := strconv.ParseFloat(strings.TrimSpace(part[eq+1:]), 64)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("--prices entry %q: bad price", part)
		}
		out[name] = v
	}
	return out, nil
}

// proxmoxConfig is the resolved connection config for the real backend.
type proxmoxConfig struct {
	APIURL         string // https://host:8006/api2/json
	TokenID        string // USER@REALM!TOKENID
	TokenSecret    string // the token UUID secret
	CAFile         string // PEM CA bundle to verify the Proxmox API cert
	TLSFingerprint string // pinned SHA-256 cert fingerprint (hex, ':' optional)
	Pool           string // optional resource pool to place clones in
}

// readTokenSecret resolves the API token secret from either the inline value or
// a file (the file form is preferred so the secret never appears in a process
// arg list). Exactly one source is used; the file wins when both are set.
func readTokenSecret(inline, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read --proxmox-token-file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(inline), nil
}

// httpClient builds the *http.Client the go-proxmox client dials the Proxmox API
// with. The TLS connection to the Proxmox API is the secret channel (it carries
// the bootstrap join material over the guest agent), so it MUST be verified.
// Verification is anchored on EITHER:
//   - a pinned CA bundle (CAFile) — the Proxmox cluster CA at
//     /etc/pve/pve-root-ca.pem, supplied by the operator; or
//   - a pinned SHA-256 certificate fingerprint (TLSFingerprint), checked against
//     the leaf certificate the server presents.
//
// A self-signed Proxmox cert is verified by trusting the operator-supplied CA /
// fingerprint — NEVER by skipping verification. There is deliberately no
// InsecureSkipVerify path anywhere in this provider.
func (c proxmoxConfig) httpClient() (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	switch {
	case c.CAFile != "":
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read --proxmox-ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates parsed from --proxmox-ca-file %s", c.CAFile)
		}
		tlsCfg.RootCAs = pool
		if c.TLSFingerprint != "" {
			fp, err := normalizeFingerprint(c.TLSFingerprint)
			if err != nil {
				return nil, err
			}
			tlsCfg.VerifyPeerCertificate = pinnedFingerprintVerifier(fp)
		}
	case c.TLSFingerprint != "":
		// Fingerprint-only pinning: defeat the default chain check (a self-signed
		// Proxmox cert has no trusted chain) but pin the exact leaf certificate by
		// its SHA-256 fingerprint. This is verification, not skipping it — an
		// unexpected cert is rejected. InsecureSkipVerify is set ONLY to bypass the
		// hostname/chain check that the explicit fingerprint check replaces; the
		// VerifyConnection callback below rejects any cert whose fingerprint does
		// not match, so there is no trust-anything window.
		fp, err := normalizeFingerprint(c.TLSFingerprint)
		if err != nil {
			return nil, err
		}
		tlsCfg.InsecureSkipVerify = true // replaced by the explicit fingerprint pin below
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("proxmox TLS: server presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != fp {
				return fmt.Errorf("proxmox TLS: server certificate fingerprint does not match the pinned --proxmox-tls-fingerprint")
			}
			return nil
		}
	default:
		return nil, fmt.Errorf("TLS verification material is required: set --proxmox-ca-file (the Proxmox cluster CA, e.g. /etc/pve/pve-root-ca.pem) or --proxmox-tls-fingerprint; the provider never skips TLS verification")
	}

	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

// pinnedFingerprintVerifier returns a VerifyPeerCertificate callback that, in
// ADDITION to the standard CA-chain verification, requires the leaf certificate
// to match the pinned SHA-256 fingerprint.
func pinnedFingerprintVerifier(fp string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("proxmox TLS: server presented no certificate")
		}
		sum := sha256.Sum256(rawCerts[0])
		if hex.EncodeToString(sum[:]) != fp {
			return fmt.Errorf("proxmox TLS: server certificate fingerprint does not match the pinned --proxmox-tls-fingerprint")
		}
		return nil
	}
}

// normalizeFingerprint canonicalises a SHA-256 fingerprint to lowercase hex with
// no separators. It accepts the common ':'-separated form
// ("AB:CD:...") and bare hex.
func normalizeFingerprint(s string) (string, error) {
	clean := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	if len(clean) != sha256.Size*2 {
		return "", fmt.Errorf("--proxmox-tls-fingerprint must be a SHA-256 hash (%d hex chars), got %d", sha256.Size*2, len(clean))
	}
	if _, err := hex.DecodeString(clean); err != nil {
		return "", fmt.Errorf("--proxmox-tls-fingerprint is not valid hex: %w", err)
	}
	return clean, nil
}

// zonesSorted returns the sorted node names (zones).
func zonesSorted(nodes []string) []string {
	out := append([]string(nil), nodes...)
	sort.Strings(out)
	return out
}
