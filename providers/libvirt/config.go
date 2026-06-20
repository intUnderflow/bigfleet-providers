package main

import (
	"encoding/json"
	"fmt"
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
//   - A single bare URI ("qemu:///system" or "qemu+ssh://user@host/system"),
//     which is assigned to the --default-zone.
//   - A comma-separated list of "zone=uri" pairs for a multi-host deployment
//     ("rack1=qemu+ssh://user@a/system,rack2=qemu+ssh://user@b/system"), where
//     each zone maps Machine.zone to a specific libvirt host.
func parseConnections(connect, defaultZone string) ([]hostConn, error) {
	connect = strings.TrimSpace(connect)
	if connect == "" {
		return nil, nil
	}
	// Single bare URI (no "zone=" and no comma): assign it to the default zone.
	if !strings.Contains(connect, "=") && !strings.Contains(connect, ",") {
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
