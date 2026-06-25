// Command proxmox is the BigFleet CapacityProvider for Proxmox VE (QEMU/KVM VMs
// on a Proxmox VE cluster). It implements the substrate-specific
// providerkit.Backend; providerkit.Server wraps it with the full
// bigfleet.v1alpha1.CapacityProvider contract (fencing, idempotency, async
// dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per Proxmox cluster (one zone per cluster node). Production uses
// the real go-proxmox backend (--proxmox-api-url + an API token); the in-memory
// fake backend (--proxmox-backend=fake, or `auto` with no --proxmox-api-url)
// backs dev and the credential-free certification run.
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
	"strings"
	"syscall"
	"time"

	"github.com/luthermonson/go-proxmox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"

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
		addr        = flag.String("addr", ":9000", "gRPC listen address")
		providerLbl = flag.String("provider", "proxmox", "provider label stamped on HostRefs (e.g. proxmox-dc1)")
		backendSel  = flag.String("proxmox-backend", "auto", "proxmox | fake | auto (auto = proxmox when --proxmox-api-url is set; else refuses to start unless --use-fake-backend is passed)")
		useFake     = flag.Bool("use-fake-backend", false, "run the credential-free in-memory fake backend (testing/conformance only — it never creates real cloud resources)")

		apiURL    = flag.String("proxmox-api-url", "", "Proxmox API URL, e.g. https://host:8006/api2/json")
		tokenID   = flag.String("proxmox-token-id", "", "Proxmox API token id: USER@REALM!TOKENID")
		tokenVal  = flag.String("proxmox-token-secret", "", "Proxmox API token secret (prefer --proxmox-token-file)")
		tokenFile = flag.String("proxmox-token-file", "", "file holding the Proxmox API token secret")
		caFile    = flag.String("proxmox-ca-file", "", "PEM CA bundle verifying the Proxmox API cert (e.g. /etc/pve/pve-root-ca.pem)")
		tlsFinger = flag.String("proxmox-tls-fingerprint", "", "pinned SHA-256 fingerprint of the Proxmox API cert (alternative to --proxmox-ca-file)")
		pool      = flag.String("proxmox-pool", "", "Proxmox resource pool to place cloned VMs in (least-privilege scope)")

		nodes       = flag.String("nodes", "", "comma-separated Proxmox node names (each a BigFleet zone); default: fake synth zones")
		defaultZone = flag.String("default-zone", "pve", "zone seed for the fake backend's two synthetic zones")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")

		typesFile  = flag.String("instance-types", "", "path to a JSON instance-type catalog (name -> {vcpu, memory_mib, template_vmid}); default: built-in pve.* sizes")
		templateVM = flag.Int("template-vmid", defaultTemplateVMID, "default source template VMID the default catalog clones from")
		prices     = flag.String("prices", "", "explicit USD/hour per instance type as type=usd pairs (default: synthetic per-vCPU/GiB pricing)")
		perVCPU    = flag.Float64("price-per-vcpu-hour", defaultPerVCPUHour, "synthetic USD/hour per vCPU when no explicit price is set")
		perGiB     = flag.Float64("price-per-gib-hour", defaultPerGiBHour, "synthetic USD/hour per GiB RAM when no explicit price is set")

		bootstrapPath = flag.String("bootstrap-path", "/run/bigfleet-bootstrap", "in-guest path the bootstrap blob is written to before it is executed (real backend)")
		bootstrapExec = flag.String("bootstrap-exec", "/bin/sh", "comma-separated argv that runs the bootstrap (the path is appended as the final arg)")
		reconcile     = flag.Duration("reconcile-interval", 2*time.Minute, "background Proxmox->inventory reconcile interval (0 = off)")

		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")
		metricsAddr = flag.String("metrics-addr", ":9090", "address for /metrics, /healthz, /readyz (empty = disabled)")
		reflectFlag = flag.Bool("reflection", true, "register gRPC server reflection (for grpcurl/debugging)")

		tlsCert = flag.String("tls-cert", "", "server certificate (PEM) for the gRPC listener")
		tlsKey  = flag.String("tls-key", "", "server private key (PEM) for the gRPC listener")
		tlsCA   = flag.String("tls-ca", "", "client CA bundle (PEM); enables mTLS on the gRPC listener")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	nodeList, err := parseNodes(*nodes)
	if err != nil {
		return err
	}

	// Instance-type catalog.
	typeMap, err := loadInstanceTypes(*typesFile)
	if err != nil {
		return err
	}
	catalog := newInstanceCatalog(typeMap, *templateVM)

	priceOverrides, err := parsePriceOverrides(*prices)
	if err != nil {
		return err
	}
	pr := newPricing(catalog, *perVCPU, *perGiB, priceOverrides)

	// Pick the Proxmox client.
	mode := resolveBackendMode(*backendSel, *apiURL)
	if *useFake {
		mode = "fake"
	} else if mode == "fake" && strings.ToLower(*backendSel) != "fake" {
		return fmt.Errorf("refusing to start the proxmox provider on the in-memory fake backend: no credentials were detected. Configure the real backend, or pass --use-fake-backend to run the credential-free fake (testing/conformance only — it never creates real resources)")
	}
	var client proxmoxClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake Proxmox backend (dev / certification only) — no real VMs will be created")
		client = newProxmoxFake()
	case "proxmox":
		secret, err := readTokenSecret(*tokenVal, *tokenFile)
		if err != nil {
			return err
		}
		if *apiURL == "" || *tokenID == "" || secret == "" {
			return errors.New("the real Proxmox backend requires --proxmox-api-url, --proxmox-token-id, and a token secret (--proxmox-token-secret/--proxmox-token-file)")
		}
		cfg := proxmoxConfig{
			APIURL:         *apiURL,
			TokenID:        *tokenID,
			TokenSecret:    secret,
			CAFile:         *caFile,
			TLSFingerprint: *tlsFinger,
			Pool:           *pool,
		}
		hc, err := cfg.httpClient()
		if err != nil {
			return err
		}
		pxClient := proxmox.NewClient(cfg.APIURL,
			proxmox.WithHTTPClient(hc),
			proxmox.WithAPIToken(cfg.TokenID, cfg.TokenSecret),
		)
		real, err := newProxmoxReal(proxmoxRealConfig{
			Client:        pxClient,
			Pool:          cfg.Pool,
			BootstrapPath: *bootstrapPath,
			BootstrapExec: splitArgv(*bootstrapExec),
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--proxmox-backend must be proxmox, fake, or auto (got %q)", *backendSel)
	}
	defer func() { _ = client.Close() }()

	// Instrument every Proxmox API call.
	m := newMetrics()
	client = newMetricsProxmoxClient(client, m)

	// Offerings: the nodes (zones) the default offerings spread across come from
	// --nodes (real) or two synthetic zones (fake).
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		nodeA, nodeB := defaultZones(nodeList, *defaultZone)
		offs = defaultOfferings(*seedCount, nodeA, nodeB, catalog.names())
	}

	// With the real backend, every offering must place onto a configured --nodes
	// node, or Create would only fail at runtime after the provider is already
	// serving. Fail fast at startup instead.
	if mode == "proxmox" {
		if err := validateOfferingNodes(offs, nodeList); err != nil {
			return err
		}
	}

	backend, err := newProxmoxBackend(*providerLbl, client, offs, catalog, pr, logger)
	if err != nil {
		return err
	}

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv, err := providerkit.New(backend, store, providerkit.Options{
		// Multi-zone provider (zone = Proxmox node): require a zone on every
		// machine so a missing zone is caught at startup, not as mis-placement.
		RequireZone: true,
		Logger:      logger,
		Timeouts: providerkit.Timeouts{
			// Clone-from-template + boot + guest-agent-reachable can be minutes on
			// slow storage; size to the backend's worst case, not a request timeout.
			Create:    8 * time.Minute,
			Configure: 8 * time.Minute,
			Drain:     15 * time.Minute, // strict PDBs can take a while
			Delete:    5 * time.Minute,
		},
	})
	if err != nil {
		return fmt.Errorf("build provider: %w", err)
	}

	creds, err := serverCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		return err
	}
	gs, healthSrv := buildGRPCServer(creds, m, *reflectFlag, logger)
	srv.Register(gs)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	// Observability HTTP server (/metrics, /healthz, /readyz).
	var obs *observabilityServer
	if *metricsAddr != "" {
		obs = newObservabilityServer(*metricsAddr, m)
		obs.start(logger)
	}

	// Background loop: Proxmox->inventory reconcile (the persisted store is the
	// primary restart path; this catches drift while running).
	go runReconciler(ctx, srv, m, *reconcile, logger)

	// Mark ready: serving traffic + probes go green.
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if obs != nil {
		obs.setReady(true)
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		if obs != nil {
			obs.setReady(false)
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			obs.stop(sctx)
			scancel()
		}
		gs.GracefulStop()
	}()

	logger.Info("serving CapacityProvider",
		"addr", lis.Addr().String(), "provider", *providerLbl,
		"proxmox_backend", mode, "security", securityMode(creds, *tlsCA),
		"offerings", len(offs), "zones", strings.Join(zonesSorted(zonesOf(offs)), ","),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads Proxmox truth into kit inventory (new
// offerings, orphans). The persisted store is the primary restart path; this
// catches drift while running.
func runReconciler(ctx context.Context, srv *providerkit.Server, m *metrics, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := srv.Reconcile(rctx)
			cancel()
			outcome := "success"
			if err != nil {
				outcome = "error"
				logger.Warn("reconcile failed", "err", err)
			}
			m.reconcile.WithLabelValues(outcome).Inc()
		}
	}
}

// resolveBackendMode picks the Proxmox client: auto resolves to the real backend
// when --proxmox-api-url is set, else the fake. This is what makes the
// credential-free certification run (which boots the binary with no
// --proxmox-api-url) default to the fake, while a real deployment gets the real
// backend.
func resolveBackendMode(flagVal, apiURL string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if strings.TrimSpace(apiURL) != "" {
			return "proxmox"
		}
		return "fake"
	default:
		return strings.ToLower(flagVal)
	}
}

// validateOfferingNodes rejects an offering placed on a node that is not in
// --nodes, so a typo fails fast at startup rather than as a runtime Create error.
func validateOfferingNodes(offs []offering, nodes []string) error {
	if len(nodes) == 0 {
		return errors.New("the real Proxmox backend requires --nodes (the cluster node names, each a BigFleet zone)")
	}
	set := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		set[n] = true
	}
	for _, off := range offs {
		if !set[off.Zone] {
			return fmt.Errorf("offering %s is placed on node %q, which is not in --nodes (%s)", off.InstanceType, off.Zone, strings.Join(nodes, ", "))
		}
	}
	return nil
}

// defaultZones returns two node/zone names to spread the default offerings
// across. With configured --nodes it uses the first two (deduped to one if only
// a single node); with none (the fake backend) it synthesises two zones so a
// conformance run exercises multi-zone placement.
func defaultZones(nodes []string, defaultZone string) (string, string) {
	switch {
	case len(nodes) >= 2:
		return nodes[0], nodes[1]
	case len(nodes) == 1:
		return nodes[0], nodes[0]
	default:
		return defaultZone + "-a", defaultZone + "-b"
	}
}

// zonesOf returns the distinct zones across a set of offerings.
func zonesOf(offs []offering) []string {
	seen := map[string]bool{}
	var out []string
	for _, off := range offs {
		if !seen[off.Zone] {
			seen[off.Zone] = true
			out = append(out, off.Zone)
		}
	}
	return out
}

// splitArgv splits a comma-separated argv string, trimming empties.
func splitArgv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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

func securityMode(creds credentials.TransportCredentials, caFile string) string {
	if creds == nil {
		return "insecure"
	}
	if caFile != "" {
		return "mTLS"
	}
	return "TLS"
}

// serverCredentials builds gRPC transport credentials from the TLS flags for the
// listener BigFleet dials. Returns nil (insecure) when no cert/key is supplied —
// acceptable only for trusted in-cluster traffic; use mTLS in production.
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
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
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
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(cfg), nil
}
