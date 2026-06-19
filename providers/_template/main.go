// Command _template is the copy-me BigFleet capacity provider.
//
// It wires a substrate-specific [templateBackend] into
// providerkit.Server (which implements the full
// bigfleet.v1alpha1.CapacityProvider contract) and serves it over gRPC. To
// create a real provider:
//
//  1. cp -r providers/_template providers/<name>
//  2. implement the TODO-marked methods in backend.go against your substrate
//  3. register <name> in the Makefile + CI matrix (automatic via the
//     providers/* glob) and get `make conformance-<name>` green.
//
// Flags:
//
//	--addr        gRPC listen address (default :9000)
//	--provider    provider/region name stamped on HostRefs (default "example")
//	--state       path to the durable state file; empty = in-memory only
//	--seed-count  number of Speculative slots to seed on first boot (default 32)
//	--tls-cert    server certificate (PEM); enables TLS when set with --tls-key
//	--tls-key     server private key (PEM)
//	--tls-ca      client CA bundle (PEM); enables mTLS (requires client certs)
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func main() {
	if err := run(); err != nil {
		slog.Error("provider exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", ":9000", "gRPC listen address")
		provider  = flag.String("provider", "example", "provider/region name stamped on HostRefs")
		statePath = flag.String("state", "", "path to the durable state file (empty = in-memory only)")
		seedCount = flag.Int("seed-count", 32, "number of Speculative slots to seed on first boot")
		tlsCert   = flag.String("tls-cert", "", "server certificate (PEM)")
		tlsKey    = flag.String("tls-key", "", "server private key (PEM)")
		tlsCA     = flag.String("tls-ca", "", "client CA bundle (PEM); enables mTLS")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Choose a Store. A real provider should always pass --state so fence
	// marks, the idempotency map and the cluster binding + shard_metadata
	// survive a restart; the in-memory store is for ephemeral / test runs.
	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	backend := &templateBackend{
		providerName: *provider,
		seeds:        seedInventory(*seedCount, *provider),
	}

	srv, err := providerkit.New(backend, store, providerkit.Options{
		// SPOT/multi-zone providers may want RequireZone: true. The template
		// sets a zone but does not require one (single-zone-compatible).
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("build provider: %w", err)
	}

	creds, err := serverCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		return err
	}
	var opts []grpc.ServerOption
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}
	gs := grpc.NewServer(opts...)
	srv.Register(gs)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		gs.GracefulStop()
	}()

	mode := "insecure"
	if creds != nil {
		mode = "TLS"
		if *tlsCA != "" {
			mode = "mTLS"
		}
	}
	logger.Info("serving CapacityProvider", "addr", lis.Addr().String(), "provider", *provider, "security", mode, "seeded", *seedCount)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func buildStore(path string) (providerkit.Store, error) {
	if path == "" {
		return providerkit.NewMemStore(), nil
	}
	store, err := providerkit.NewFileStore(path)
	if err != nil {
		return nil, fmt.Errorf("open state file %s: %w", path, err)
	}
	return store, nil
}

// serverCredentials builds gRPC transport credentials from the TLS flags.
// Returns nil (insecure) when no cert/key is supplied — fine for in-cluster
// trust, but use mTLS in production.
func serverCredentials(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	if certFile == "" && keyFile == "" {
		if caFile != "" {
			return nil, errors.New("--tls-ca set without --tls-cert/--tls-key")
		}
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("both --tls-cert and --tls-key are required to enable TLS")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("no certificates parsed from client CA %s", caFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert // mTLS
	}
	return credentials.NewTLS(cfg), nil
}
