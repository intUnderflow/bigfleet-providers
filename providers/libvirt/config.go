package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// instanceTypeSpec is the JSON shape of one catalog entry in an --instance-types
// file: a flavor name mapping to its hardware.
type instanceTypeSpec struct {
	VCPU      int   `json:"vcpu"`
	MemoryMiB int64 `json:"memory_mib"`
}

// loadInstanceTypes reads an instance-type catalog from a JSON file (an object
// of name -> {vcpu, memory_mib}). Empty path returns the built-in default.
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
		out[name] = vmCapacity{VCPU: spec.VCPU, MemMiB: spec.MemoryMiB}
	}
	return out, nil
}

// hostConn pairs a zone (BigFleet zone label) with the libvirt connection URI
// for the host that backs it.
type hostConn struct {
	Zone string
	URI  string
}

// parseConnections parses the --connect flag into a zone -> URI map.
//
// Two forms are accepted:
//   - A single bare URI ("qemu:///system" or "qemu+libssh://user@host/system"),
//     which is assigned to the --default-zone.
//   - A comma-separated list of "zone=uri" pairs for a multi-host deployment
//     ("rack1=qemu+libssh://user@a/system,rack2=qemu+libssh://user@b/system"),
//     where each zone maps Machine.zone to a specific libvirt host.
//
// For SSH, use the "qemu+libssh://" scheme (libvirt's pure-Go SSH transport),
// not "qemu+ssh://": the pinned go-libvirt accepts the keyfile/known_hosts URI
// parameters only on the libssh transport. The string is passed verbatim to
// go-libvirt's ConnectToURI; this only splits zones from URIs.
func parseConnections(connect, defaultZone string) ([]hostConn, error) {
	connect = strings.TrimSpace(connect)
	if connect == "" {
		return nil, nil
	}
	// A single entry (no comma) that is a bare URI is assigned to the default
	// zone. We distinguish a bare URI from a "zone=uri" assignment by the first
	// '=': in a zone=uri entry the text before it is a plain zone label (no ':'
	// or '/'); in a bare URI any '=' lives inside the URI (the scheme's "://" or
	// a "?key=val" query), so its prefix contains ':' or '/'. This lets a
	// single-host bare URI carry query params like
	// "?keyfile=...&known_hosts=..." without being mis-split into zone=uri.
	if !strings.Contains(connect, ",") && isBareURI(connect) {
		if err := validateConnectURI(connect); err != nil {
			return nil, err
		}
		return []hostConn{{Zone: defaultZone, URI: connect}}, nil
	}
	var out []hostConn
	seen := map[string]bool{}
	for _, part := range strings.Split(connect, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			return nil, fmt.Errorf("--connect entry %q must be zone=uri", part)
		}
		zone := strings.TrimSpace(part[:eq])
		uri := strings.TrimSpace(part[eq+1:])
		if zone == "" || uri == "" {
			return nil, fmt.Errorf("--connect entry %q must be zone=uri", part)
		}
		if strings.ContainsAny(zone, "/") {
			return nil, fmt.Errorf("--connect zone %q must not contain '/'", zone)
		}
		if err := validateConnectURI(uri); err != nil {
			return nil, err
		}
		if seen[zone] {
			return nil, fmt.Errorf("--connect lists zone %q twice", zone)
		}
		seen[zone] = true
		out = append(out, hostConn{Zone: zone, URI: uri})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--connect %q parsed to no host connections", connect)
	}
	return out, nil
}

// isBareURI reports whether a single --connect entry is a bare libvirt URI (to
// be assigned to the default zone) rather than a "zone=uri" assignment. A
// libvirt URI always has a scheme, so the text before its first '=' contains a
// ':' (e.g. "qemu+libssh://h/system?keyfile" or "qemu:///system"); a zone=uri
// entry's prefix is a plain zone label with no ':'. An entry with no '=' at all
// is a bare URI.
func isBareURI(s string) bool {
	eq := strings.Index(s, "=")
	if eq < 0 {
		return true
	}
	return strings.Contains(s[:eq], ":")
}

// validateConnectURI rejects a connect URI whose transport would carry the
// cluster-join secret without authenticated encryption, or with peer
// verification turned off. The bootstrap blob is delivered over this same
// connection (qemu guest-exec), so:
//   - ssh/libssh/libssh2: require strict host-key verification
//     (known_hosts_verify=normal, the default); 'auto' (trust-on-first-use) and
//     'ignore' are refused, as is no_verify.
//   - tls: refuse no_verify (which sets InsecureSkipVerify).
//   - tcp: refuse outright — plaintext, no authentication.
//
// A remote bare URI (qemu://host/system, no +transport) is dialed over TLS by
// go-libvirt, so it is validated as tls. The local unix socket carries no peer
// identity and passes.
func validateConnectURI(uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("invalid --connect URI %q: %w", uri, err)
	}
	transport := ""
	if parts := strings.SplitN(u.Scheme, "+", 2); len(parts) == 2 {
		transport = parts[1]
	} else if u.Host != "" {
		transport = "tls" // go-libvirt dials a remote bare URI over TLS
	}
	q := u.Query()
	switch transport {
	case "ssh", "libssh", "libssh2":
		switch q.Get("known_hosts_verify") {
		case "", "normal":
		default:
			return fmt.Errorf("--connect URI %q must use known_hosts_verify=normal (strict against known_hosts); 'auto' (trust-on-first-use) and 'ignore' leave a host-key MITM window", uri)
		}
		if noVerifyTruthy(q.Get("no_verify")) {
			return fmt.Errorf("--connect URI %q sets no_verify, disabling SSH host-key verification; remove it", uri)
		}
	case "tls":
		if noVerifyTruthy(q.Get("no_verify")) {
			return fmt.Errorf("--connect URI %q sets no_verify, disabling TLS server-certificate verification; remove it", uri)
		}
	case "tcp":
		return fmt.Errorf("--connect URI %q uses plaintext qemu+tcp:// (no encryption or authentication); the cluster-join secret would travel in the clear — use qemu+tls:// or qemu+libssh://", uri)
	}
	return nil
}

// noVerifyTruthy reports whether a libvirt "no_verify" query value is enabled
// (go-libvirt treats any non-zero integer as on).
func noVerifyTruthy(v string) bool { return v != "" && v != "0" }

// zones returns the sorted zone names across a set of host connections.
func zonesOf(conns []hostConn) []string {
	out := make([]string, 0, len(conns))
	for _, c := range conns {
		out = append(out, c.Zone)
	}
	sort.Strings(out)
	return out
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
