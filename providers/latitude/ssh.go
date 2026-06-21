package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshDelivery delivers commands to a deployed server over SSH, verifying the
// host key against the fingerprint pinned at deploy. It NEVER uses
// ssh.InsecureIgnoreHostKey — a host whose key does not match the pin fails
// closed, so the (secret-bearing) bootstrap is never delivered over an
// unauthenticated channel.
type sshDelivery struct {
	signer ssh.Signer
	user   string
	// onTOFU persists a host-key fingerprint observed on first use for a server
	// that had no pin (an orphan / pre-pinning server), so later connections are
	// verified. May be nil.
	onTOFU func(serverID, fp string)
}

// loadSSHSigner reads and parses an SSH private key from disk.
func loadSSHSigner(path string) (ssh.Signer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --ssh-key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse --ssh-key %s: %w", path, err)
	}
	return signer, nil
}

// run dials srv, runs script over a single session, and returns an error unless
// it exits 0. The server's SSH host key is verified against srv.HostKeyFP (the
// fingerprint pinned at deploy); a mismatch aborts the connection as a possible
// MITM.
func (d *sshDelivery) run(ctx context.Context, srv serverInstance, script string) error {
	host := srv.PublicIPv4
	if host == "" {
		return fmt.Errorf("ssh: no public IPv4 for server %s", srv.ServerID)
	}
	if d.signer == nil {
		return fmt.Errorf("ssh: no key configured (set --ssh-key)")
	}
	var tofuFP string
	cfg := &ssh.ClientConfig{
		User:            d.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.signer)},
		HostKeyCallback: hostKeyCallback(srv.HostKeyFP, func(fp string) { tofuFP = fp }),
		Timeout:         15 * time.Second,
	}
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "22"))
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", host, err)
	}
	defer func() { _ = conn.Close() }()
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(host, "22"), cfg)
	if err != nil {
		return fmt.Errorf("ssh handshake %s: %w", host, err)
	}
	if srv.HostKeyFP == "" && tofuFP != "" && d.onTOFU != nil {
		d.onTOFU(srv.ServerID, tofuFP)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session %s: %w", host, err)
	}
	defer func() { _ = session.Close() }()

	done := make(chan error, 1)
	go func() { done <- session.Run(script) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return fmt.Errorf("ssh command on %s did not complete: %w", host, ctx.Err())
	case err := <-done:
		if err != nil {
			return fmt.Errorf("ssh command on %s failed: %w", host, err)
		}
		return nil
	}
}

// shellQuote single-quotes a string for safe interpolation into a /bin/sh
// command (the blob and cluster id are opaque, so never trust their bytes).
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}
