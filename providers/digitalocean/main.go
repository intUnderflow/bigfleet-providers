// Command digitalocean is the BigFleet CapacityProvider for DigitalOcean
// Droplets. It implements the substrate-specific providerkit.Backend;
// providerkit.Server wraps it with the full bigfleet.v1alpha1.CapacityProvider
// contract (fencing, idempotency, async dispatch, transition timeouts,
// shard_metadata, field-shape).
//
// One process per DigitalOcean region. Production uses the real DigitalOcean
// backend (--token / DIGITALOCEAN_TOKEN + --region); the in-memory fake backend
// (--do-backend=fake, or `auto` with no token/region) backs dev and the
// credential-free conformance / certification run.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
		providerLbl = flag.String("provider", "digitalocean", "provider/region label stamped on HostRefs (e.g. digitalocean-nyc3)")
		backendSel  = flag.String("do-backend", "auto", "digitalocean | fake | auto (auto = digitalocean when a token AND region are set, else fake)")
		token       = flag.String("token", "", "DigitalOcean Personal Access Token (or set DIGITALOCEAN_TOKEN)")
		region      = flag.String("region", "", "DigitalOcean region slug this process serves (e.g. nyc3); required for the digitalocean backend")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		regionA     = flag.String("region-a", "nyc3", "first region for default offerings")
		regionB     = flag.String("region-b", "sfo3", "second region for default offerings")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		image        = flag.String("image", "", "base image / snapshot slug or id for Droplets.Create (digitalocean backend)")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init baked in at Droplet create (installs the on-host agent)")
		priceRefresh = flag.Duration("price-refresh", 30*time.Minute, "price refresh interval (0 = off)")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background DigitalOcean->inventory reconcile interval (0 = off)")

		// On-host agent bootstrap channel (§4.6 (A)). Used only by the real backend.
		bootstrapAddr     = flag.String("bootstrap-addr", "", "address to serve the on-host agent bootstrap channel (HTTPS); empty disables it")
		bootstrapEndpoint = flag.String("bootstrap-endpoint", "", "externally-reachable URL of the bootstrap channel, injected into Droplet user_data (e.g. https://do-provider.example:9443)")
		bootstrapCert     = flag.String("bootstrap-tls-cert", "", "server certificate (PEM) for the bootstrap channel")
		bootstrapKey      = flag.String("bootstrap-tls-key", "", "server private key (PEM) for the bootstrap channel")
		bootstrapCA       = flag.String("bootstrap-ca", "", "CA bundle (PEM) the on-host agent pins to verify the provider (default: the server cert)")
		bootstrapSecret   = flag.String("bootstrap-secret", "", "HMAC secret minting per-machine agent tokens (or set BIGFLEET_BOOTSTRAP_SECRET; random if unset)")

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

	doToken := *token
	if doToken == "" {
		doToken = os.Getenv("DIGITALOCEAN_TOKEN")
	}

	m := newMetrics()

	// The bootstrap agent channel is required by the real backend; build it (and
	// serve it) only when the real backend is selected.
	mode := resolveBackendMode(*backendSel, doToken, *region)

	var (
		client  doClient
		vault   *bootstrapVault
		obsBoot *http.Server
	)
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake DigitalOcean backend (dev / conformance only) — no real Droplets will be created")
		client = newDOFake()
	case "digitalocean":
		secret, err := resolveBootstrapSecret(*bootstrapSecret)
		if err != nil {
			return err
		}
		vault = newBootstrapVault(secret, logger)
		caPEM, srv, err := startBootstrapChannel(*bootstrapAddr, *bootstrapCert, *bootstrapKey, *bootstrapCA, vault, logger)
		if err != nil {
			return err
		}
		obsBoot = srv
		real, err := newDOReal(doRealConfig{
			Token:             doToken,
			Region:            *region,
			Image:             *image,
			Vault:             vault,
			BootstrapEndpoint: *bootstrapEndpoint,
			BootstrapCAPEM:    caPEM,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--do-backend must be digitalocean, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every DigitalOcean API call.
	client = newMetricsDOClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		offs = defaultOfferings(*seedCount, *regionA, *regionB)
	}

	var userData []byte
	if *baseUserData != "" {
		b, err := os.ReadFile(*baseUserData)
		if err != nil {
			return fmt.Errorf("read --base-user-data: %w", err)
		}
		userData = b
	}

	pr := newPricing(client, logger)
	backend, err := newDigitaloceanBackend(*providerLbl, *image, client, offs, pr, userData, logger)
	if err != nil {
		return err
	}

	// Warm the price cache before first List (best-effort, bounded).
	warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Second)
	backend.refreshPrices(warmCtx)
	cancelWarm()

	// Resolve allocatable (vCPU/memory) for the offered sizes from the Sizes API
	// (best-effort, bounded); the pinned table covers anything DigitalOcean can't
	// return. Specs are immutable, so this runs once.
	szCtx, cancelSZ := context.WithTimeout(ctx, 20*time.Second)
	if missed := backend.refreshSizes(szCtx); missed > 0 {
		logger.Warn("some offered sizes unresolved from DigitalOcean; using pinned table", "unresolved", missed)
	}
	cancelSZ()

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv, err := providerkit.New(backend, store, providerkit.Options{
		// Multi-region provider: require a zone (region) on every machine.
		RequireZone: true,
		Logger:      logger,
		Timeouts: providerkit.Timeouts{
			// DigitalOcean Droplet create + boot + kubelet-ready: minutes, not seconds.
			Create:    5 * time.Minute,
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

	// Background loops: price refresh + DigitalOcean->inventory reconcile.
	go runPriceRefresher(ctx, backend, m, *priceRefresh)
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
		if obsBoot != nil {
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = obsBoot.Shutdown(sctx)
			scancel()
		}
		gs.GracefulStop()
	}()

	logger.Info("serving CapacityProvider",
		"addr", lis.Addr().String(), "provider", *providerLbl,
		"do_backend", mode, "region", *region, "security", securityMode(creds, *tlsCA),
		"offerings", len(offs), "metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// startBootstrapChannel starts the HTTPS server that serves the on-host agent
// control channel and returns the CA PEM the agent should pin (the explicit
// --bootstrap-ca, else the server certificate). It requires a cert/key; the blob
// it carries is a join secret, so the channel is always TLS.
func startBootstrapChannel(addr, certFile, keyFile, caFile string, vault *bootstrapVault, logger *slog.Logger) (string, *http.Server, error) {
	if addr == "" || certFile == "" || keyFile == "" {
		return "", nil, fmt.Errorf("the digitalocean backend requires --bootstrap-addr, --bootstrap-tls-cert and --bootstrap-tls-key (the bootstrap blob is a join secret and must travel over TLS)")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return "", nil, fmt.Errorf("load bootstrap TLS keypair: %w", err)
	}
	caBytes, err := os.ReadFile(firstNonEmpty(caFile, certFile))
	if err != nil {
		return "", nil, fmt.Errorf("read bootstrap CA: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", vault)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	go func() {
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("bootstrap channel server", "err", err)
		}
	}()
	logger.Info("serving on-host agent bootstrap channel", "addr", addr)
	return string(caBytes), srv, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// resolveBootstrapSecret reads the HMAC secret from the flag, then the
// environment, generating a random one (with a warning) if neither is set. A
// random secret works within one process lifetime but invalidates already-issued
// agent tokens on restart, so production should pin it.
func resolveBootstrapSecret(flagVal string) ([]byte, error) {
	if flagVal != "" {
		return []byte(flagVal), nil
	}
	if env := os.Getenv("BIGFLEET_BOOTSTRAP_SECRET"); env != "" {
		return []byte(env), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate bootstrap secret: %w", err)
	}
	slog.Warn("no --bootstrap-secret / BIGFLEET_BOOTSTRAP_SECRET set: using a random secret (agent tokens won't survive a provider restart; pin one in production)")
	return []byte(hex.EncodeToString(buf)), nil
}

// runReconciler periodically re-reads DigitalOcean truth into kit inventory (new
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

func runPriceRefresher(ctx context.Context, backend *digitaloceanBackend, m *metrics, interval time.Duration) {
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
		}
	}
}

// resolveBackendMode picks the substrate client. auto = the real DigitalOcean
// backend when BOTH a token and a region are set, otherwise the fake — so a
// credential-free run (no token, no region; exactly how certification boots the
// binary) defaults to fake.
func resolveBackendMode(flagVal, token, region string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if token != "" && region != "" {
			return "digitalocean"
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

func securityMode(creds credentials.TransportCredentials, caFile string) string {
	if creds == nil {
		return "insecure"
	}
	if caFile != "" {
		return "mTLS"
	}
	return "TLS"
}

// serverCredentials builds gRPC transport credentials from the TLS flags. Returns
// nil (insecure) when no cert/key is supplied — acceptable only for trusted
// in-cluster traffic; use mTLS in production.
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
