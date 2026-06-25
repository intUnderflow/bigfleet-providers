// Command azure is the BigFleet CapacityProvider for Microsoft Azure. It
// implements the substrate-specific providerkit.Backend; providerkit.Server
// wraps it with the full bigfleet.v1alpha1.CapacityProvider contract (fencing,
// idempotency, async dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per Azure region (location). Production uses the real Azure
// backend (--location + a resolvable Azure credential); the in-memory fake
// backend (--azure-backend=fake, or `auto` with no --location) backs dev and the
// credential-free conformance run.
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
		providerLbl = flag.String("provider", "azure", "provider/region label stamped on HostRefs (e.g. azure-eastus)")
		backendSel  = flag.String("azure-backend", "auto", "azure | fake | auto (auto = azure when --location is set; without it the provider refuses to start unless --use-fake-backend is passed)")
		useFake     = flag.Bool("use-fake-backend", false, "run the credential-free in-memory fake backend (testing/conformance only — it never creates real VMs)")
		location    = flag.String("location", "", "Azure region/location (required for the azure backend), e.g. eastus")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		zoneA       = flag.String("zone-a", "", "first zone for default offerings (default: <location>-1)")
		zoneB       = flag.String("zone-b", "", "second zone for default offerings (default: <location>-2)")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		subscription = flag.String("subscription-id", "", "Azure subscription id (azure backend; or AZURE_SUBSCRIPTION_ID)")
		resourceGrp  = flag.String("resource-group", "", "target resource group for VMs (azure backend)")
		subnetID     = flag.String("subnet-id", "", "VNet/subnet resource id NICs attach to (azure backend)")
		image        = flag.String("image", "Canonical:ubuntu-24_04-lts:server:latest", "VM image URN/id for Create (azure backend)")
		adminUser    = flag.String("admin-username", "bigfleet", "VM admin username (azure backend)")
		sshPubKey    = flag.String("ssh-public-key", "", "path to the SSH public key authorised for the admin user (azure backend)")
		bootstrapHk  = flag.String("bootstrap-hook", "/opt/bigfleet/bootstrap", "image path that applies the delivered bootstrap blob")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init baked into customData at create")
		priceRefresh = flag.Duration("price-refresh", time.Hour, "on-demand + spot price refresh interval (0 = off)")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background Azure->inventory reconcile interval (0 = off)")
		evictionTok  = flag.String("eviction-token", "", "shared bearer token the node-side Scheduled Events agent presents to POST /internal/eviction (or set BIGFLEET_EVICTION_TOKEN, preferred — Secret-mounted). Empty = the endpoint is disabled (fail-closed), not unauthenticated")

		metricsAddr = flag.String("metrics-addr", ":9090", "address for /metrics, /healthz, /readyz (empty = disabled)")
		reflectFlag = flag.Bool("reflection", true, "register gRPC server reflection (for grpcurl/debugging)")

		tlsCert = flag.String("tls-cert", "", "server certificate (PEM)")
		tlsKey  = flag.String("tls-key", "", "server private key (PEM)")
		tlsCA   = flag.String("tls-ca", "", "client CA bundle (PEM); enables mTLS")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	subID := *subscription
	if subID == "" {
		subID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	}

	// Pick the Azure client. The fake backend must be requested explicitly — it
	// never creates real resources, so a misconfigured operator (no credentials)
	// must fail closed rather than silently come up on a simulation.
	mode := resolveBackendMode(*backendSel, *location)
	if *useFake {
		mode = "fake"
	} else if mode == "fake" && strings.ToLower(*backendSel) != "fake" {
		return fmt.Errorf("refusing to start the azure provider on the in-memory fake backend: no Azure credentials / --location were detected. Configure the real backend, or pass --use-fake-backend to run the credential-free fake (testing/conformance only — it never creates real VMs)")
	}
	var client azureClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake Azure backend (dev / conformance only) — no real VMs will be created")
		client = newAzureFake()
	case "azure":
		var sshKey []byte
		if *sshPubKey != "" {
			b, err := os.ReadFile(*sshPubKey)
			if err != nil {
				return fmt.Errorf("read --ssh-public-key: %w", err)
			}
			sshKey = b
		}
		real, err := newAzureReal(azureRealConfig{
			SubscriptionID:    subID,
			ResourceGroup:     *resourceGrp,
			Location:          *location,
			SubnetID:          *subnetID,
			Image:             *image,
			AdminUsername:     *adminUser,
			SSHPublicKey:      sshKey,
			BootstrapHookPath: *bootstrapHk,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--azure-backend must be azure, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every Azure API call.
	m := newMetrics()
	client = newMetricsAzureClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		offs = defaultOfferings(*seedCount, defaultZone(*zoneA, *location, "1"), defaultZone(*zoneB, *location, "2"))
	}

	var userData []byte
	if *baseUserData != "" {
		b, err := os.ReadFile(*baseUserData)
		if err != nil {
			return fmt.Errorf("read --base-user-data: %w", err)
		}
		userData = b
	}

	pr := newPricing(*location, client, logger)
	in := newInterruption()
	backend, err := newAzureBackend(*providerLbl, *location, client, offs, pr, in, userData, logger)
	if err != nil {
		return err
	}

	// Warm the on-demand + spot price caches before first List (best-effort,
	// bounded), so the live prices are in place before serving.
	warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Second)
	backend.refreshPrices(warmCtx)
	cancelWarm()
	if ts := backend.lastPriceSuccess(); !ts.IsZero() {
		m.priceFresh.Set(float64(ts.Unix()))
	}

	// Resolve allocatable (vCPU/memory) for the offered sizes from the Resource
	// SKUs API (best-effort, bounded); the pinned table covers anything Azure
	// can't return. Specs are immutable, so this runs once.
	szCtx, cancelSz := context.WithTimeout(ctx, 20*time.Second)
	if missed := backend.refreshVMSizes(szCtx); missed > 0 {
		logger.Warn("some offered VM sizes unresolved from Azure; using pinned table", "unresolved", missed)
	}
	cancelSz()

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv, err := providerkit.New(backend, store, providerkit.Options{
		// Multi-zone provider: require a zone on every machine.
		RequireZone: true,
		Logger:      logger,
		Timeouts: providerkit.Timeouts{
			// Azure VM create + boot + kubelet-ready: minutes, not seconds.
			Create:    8 * time.Minute,
			Configure: 8 * time.Minute,
			Drain:     15 * time.Minute, // strict PDBs can take a while
			Delete:    8 * time.Minute,
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

	// Observability HTTP server (/metrics, /healthz, /readyz), plus — on the real
	// backend — the Scheduled Events eviction ingest endpoint that a node-side
	// agent POSTs Spot Preempt notices to (raising observed interruption
	// probability). The fake backend never registers it.
	var obs *observabilityServer
	if *metricsAddr != "" {
		obs = newObservabilityServer(*metricsAddr, m)
		if mode == "azure" {
			// Prefer the env var (Helm mounts it from a Secret) over the flag, so the
			// shared secret need not appear in cleartext in the pod's argv/spec.
			evToken := *evictionTok
			if evToken == "" {
				evToken = os.Getenv("BIGFLEET_EVICTION_TOKEN")
			}
			// Fail closed: the endpoint mutates interruption state and triggers a
			// reconcile, so it is only exposed when a token is configured. Without
			// one it stays unregistered rather than accepting unauthenticated POSTs.
			if evToken != "" {
				reporter := newEvictionReporter(backend, srv, m, evToken, logger)
				obs.handle("/internal/eviction", reporter.handle)
			} else {
				logger.Warn("eviction ingest endpoint /internal/eviction disabled: set --eviction-token / BIGFLEET_EVICTION_TOKEN to enable it")
			}
		}
		obs.start(logger)
	}

	// (newPricing already warns once when a region has no on-demand seed table
	// and falls back to the baseline; no need to repeat it here.)

	// Background loops: price refresh (on-demand + spot) + Azure->inventory reconcile.
	go runPriceRefresher(ctx, backend, m, *priceRefresh, logger)
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
		"addr", lis.Addr().String(), "provider", *providerLbl, "location", *location,
		"azure_backend", mode, "security", securityMode(creds, *tlsCA), "offerings", len(offs),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads Azure truth into kit inventory (new
// offerings, orphans, evicted Spot VMs). The persisted store is the primary
// restart path; this catches drift while running.
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

func runPriceRefresher(ctx context.Context, backend *azureBackend, m *metrics, interval time.Duration, logger *slog.Logger) {
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
			failed := backend.refreshPrices(rctx)
			cancel()
			outcome := "success"
			if failed > 0 {
				outcome = "error"
			}
			m.priceRefresh.WithLabelValues(outcome).Inc()
			// Surface staleness: publish the last fully-successful refresh time and
			// warn when the cache has not refreshed cleanly for several intervals.
			if ts := backend.lastPriceSuccess(); !ts.IsZero() {
				m.priceFresh.Set(float64(ts.Unix()))
				if age := time.Since(ts); age > 3*interval {
					logger.Warn("pricing: price cache going stale; last fully-successful refresh is old (still serving cached prices)",
						"age", age.Round(time.Second), "refresh_interval", interval)
				}
			}
		}
	}
}

func resolveBackendMode(flagVal, location string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if location != "" {
			return "azure"
		}
		return "fake"
	default:
		return strings.ToLower(flagVal)
	}
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

// defaultZone derives a BigFleet zone string "<location>-<n>" (e.g. eastus-1)
// from the location and an availability-zone number, or returns the override.
func defaultZone(override, location, num string) string {
	if override != "" {
		return override
	}
	if location != "" {
		return location + "-" + num
	}
	return "eastus-" + num
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

// serverCredentials builds gRPC transport credentials from the TLS flags.
// Returns nil (insecure) when no cert/key is supplied — acceptable only for
// trusted in-cluster traffic; use mTLS in production.
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
