// Command scaleway is the BigFleet CapacityProvider for Scaleway. It implements
// the substrate-specific providerkit.Backend; providerkit.Server wraps it with
// the full bigfleet.v1alpha1.CapacityProvider contract (fencing, idempotency,
// async dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per region/backend pair. Two substrates are supported, selected by
// --substrate:
//
//   - instances   → Scaleway Instances, capacity_type ON_DEMAND (cloud VMs that
//     can be torn down: implements Delete).
//   - elastic-metal → Scaleway Elastic Metal, capacity_type BARE_METAL (physical
//     servers returned to a free pool: Delete is codes.Unimplemented).
//
// Production uses the real Scaleway backend (SCW_ACCESS_KEY / SCW_SECRET_KEY +
// project/zone). The in-memory fake backend (--scaleway-backend=fake, or `auto`
// with no credentials) backs dev and the credential-free certification run.
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
		providerLbl = flag.String("provider", "scaleway", "provider/region label stamped on HostRefs (e.g. scaleway-fr-par)")
		substrate   = flag.String("substrate", "instances", "substrate served by this process: instances | elastic-metal")
		backendSel  = flag.String("scaleway-backend", "auto", "scaleway | fake | auto (auto = scaleway when credentials are set, else fake)")

		accessKey = flag.String("access-key", "", "Scaleway access key (or set SCW_ACCESS_KEY)")
		secretKey = flag.String("secret-key", "", "Scaleway secret key (or set SCW_SECRET_KEY)")
		projectID = flag.String("project-id", "", "Scaleway project id (or set SCW_DEFAULT_PROJECT_ID)")

		offerings = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		zoneA     = flag.String("zone-a", "fr-par-1", "first zone for default offerings (and the region this process serves)")
		zoneB     = flag.String("zone-b", "nl-ams-1", "second zone for default offerings")
		statePath = flag.String("state", "", "durable state file (empty = in-memory only)")

		image        = flag.String("image", "", "base image label/id for CreateServer (scaleway backend)")
		eurUSD       = flag.Float64("eur-usd", defaultEURtoUSD, "EUR->USD conversion rate applied to Scaleway prices")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init baked in at server create (installs the on-host agent)")
		priceRefresh = flag.Duration("price-refresh", 30*time.Minute, "price refresh interval (0 = off)")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background Scaleway->inventory reconcile interval (0 = off)")

		// On-host agent bootstrap channel (§4.5). Used only by the real backend.
		bootstrapAddr     = flag.String("bootstrap-addr", "", "address to serve the on-host agent bootstrap channel (HTTPS); empty disables it")
		bootstrapEndpoint = flag.String("bootstrap-endpoint", "", "externally-reachable URL of the bootstrap channel, injected into server user_data (e.g. https://scaleway-provider.example:9443)")
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

	capacity, isCloud, err := resolveSubstrate(*substrate)
	if err != nil {
		return err
	}

	creds := scwCredentials{
		accessKey: firstNonEmpty(*accessKey, os.Getenv("SCW_ACCESS_KEY")),
		secretKey: firstNonEmpty(*secretKey, os.Getenv("SCW_SECRET_KEY")),
		projectID: firstNonEmpty(*projectID, os.Getenv("SCW_DEFAULT_PROJECT_ID")),
		region:    *zoneA,
	}

	// Pick the Scaleway client. The bootstrap agent channel is required by the
	// real backend; build and serve it only when the real backend is selected (so
	// the credential-free certification run on the fake never needs TLS material).
	mode := resolveBackendMode(*backendSel, creds)
	var (
		client  scwClient
		bootSrv *http.Server
	)
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake Scaleway backend (dev / certification only) — no real servers will be created")
		client = newSCWFake()
	case "scaleway":
		secret, err := resolveBootstrapSecret(*bootstrapSecret)
		if err != nil {
			return err
		}
		vault := newBootstrapVault(secret, logger)
		caPEM, srv, err := startBootstrapChannel(*bootstrapAddr, *bootstrapCert, *bootstrapKey, *bootstrapCA, vault, logger)
		if err != nil {
			return err
		}
		bootSrv = srv
		real, err := newSCWReal(scwRealConfig{
			Creds:             creds,
			CommercialKind:    capacity,
			Image:             *image,
			Zone:              *zoneA,
			EURtoUSD:          *eurUSD,
			Vault:             vault,
			BootstrapEndpoint: *bootstrapEndpoint,
			BootstrapCAPEM:    caPEM,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--scaleway-backend must be scaleway, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every Scaleway API call.
	m := newMetrics()
	client = newMetricsSCWClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else if isCloud {
		offs = defaultInstanceOfferings(*seedCount, *zoneA, *zoneB)
	} else {
		offs = defaultBaremetalOfferings(*seedCount, *zoneA, *zoneB)
	}

	var userData []byte
	if *baseUserData != "" {
		b, err := os.ReadFile(*baseUserData)
		if err != nil {
			return fmt.Errorf("read --base-user-data: %w", err)
		}
		userData = b
	}

	pr := newPricing(*eurUSD, client, logger)
	core, err := newScalewayBackend(*providerLbl, capacity, *image, client, offs, pr, userData, logger)
	if err != nil {
		return err
	}

	// Warm the price cache before first List (best-effort, bounded).
	warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Second)
	core.refreshPrices(warmCtx)
	cancelWarm()

	// Resolve allocatable (vCPU/memory/GPU) for the offered types from the
	// Scaleway catalogue (best-effort, bounded); the pinned table covers anything
	// the catalogue can't return. Specs are immutable, so this runs once.
	stCtx, cancelST := context.WithTimeout(ctx, 20*time.Second)
	if missed := core.refreshTypes(stCtx); missed > 0 {
		logger.Warn("some offered commercial types unresolved from Scaleway; using pinned table", "unresolved", missed)
	}
	cancelST()

	// The Delete capability is substrate-specific: Instances (cloud) wraps the
	// core backend in a Deleter; Elastic Metal (free pool) uses it bare so the
	// kit answers Delete with codes.Unimplemented.
	var backend providerkit.Backend = core
	if isCloud {
		backend = &cloudBackend{scalewayBackend: core}
	}

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv, err := providerkit.New(backend, store, providerkit.Options{
		// Multi-zone provider: require a zone on every machine.
		RequireZone: true,
		Logger:      logger,
		Timeouts:    timeoutsFor(isCloud),
	})
	if err != nil {
		return fmt.Errorf("build provider: %w", err)
	}

	tcreds, err := serverCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		return err
	}
	gs, healthSrv := buildGRPCServer(tcreds, m, *reflectFlag, logger)
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

	// Background loops: price refresh + Scaleway->inventory reconcile.
	go runPriceRefresher(ctx, core, m, *priceRefresh)
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
		if bootSrv != nil {
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = bootSrv.Shutdown(sctx)
			scancel()
		}
		gs.GracefulStop()
	}()

	logger.Info("serving CapacityProvider",
		"addr", lis.Addr().String(), "provider", *providerLbl,
		"substrate", *substrate, "scaleway_backend", mode, "capacity", capacity.String(),
		"security", securityMode(tcreds, *tlsCA), "offerings", len(offs), "metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// resolveSubstrate maps --substrate to its capacity type and whether it is the
// cloud (deletable) path.
func resolveSubstrate(s string) (providerkit.CapacityType, bool, error) {
	switch strings.ToLower(s) {
	case "instances", "instance", "":
		return providerkit.CapacityOnDemand, true, nil
	case "elastic-metal", "elastic_metal", "baremetal", "bare-metal", "metal":
		return providerkit.CapacityBareMetal, false, nil
	default:
		return providerkit.CapacityUnspecified, false, fmt.Errorf("--substrate must be instances or elastic-metal (got %q)", s)
	}
}

// timeoutsFor sizes the per-transition timeouts to the substrate. Elastic Metal
// commissioning (CreateServer + InstallServer) takes tens of minutes to hours,
// so its Create/Configure timeouts are far more generous than the cloud path.
func timeoutsFor(isCloud bool) providerkit.Timeouts {
	if isCloud {
		return providerkit.Timeouts{
			Create:    5 * time.Minute,
			Configure: 8 * time.Minute,
			Drain:     15 * time.Minute, // strict PDBs can take a while
			Delete:    5 * time.Minute,
		}
	}
	// Elastic Metal: slow physical commissioning.
	return providerkit.Timeouts{
		Create:    2 * time.Hour,
		Configure: 30 * time.Minute,
		Drain:     30 * time.Minute,
		Delete:    5 * time.Minute, // unused (no Deleter), but harmless
	}
}

// runReconciler periodically re-reads Scaleway truth into kit inventory (new
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

func runPriceRefresher(ctx context.Context, backend *scalewayBackend, m *metrics, interval time.Duration) {
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

// resolveBackendMode picks scaleway vs fake. `auto` selects the real backend
// only when full API credentials are present, else the fake (so the
// credential-free certification run uses the fake).
func resolveBackendMode(flagVal string, creds scwCredentials) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if creds.complete() {
			return "scaleway"
		}
		return "fake"
	default:
		return strings.ToLower(flagVal)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

// startBootstrapChannel starts the HTTPS server that serves the on-host agent
// control channel and returns the CA PEM the agent should pin (the explicit
// --bootstrap-ca, else the server certificate). It requires a cert/key; the blob
// it carries is a join secret, so the channel is always TLS.
func startBootstrapChannel(addr, certFile, keyFile, caFile string, vault *bootstrapVault, logger *slog.Logger) (string, *http.Server, error) {
	if addr == "" || certFile == "" || keyFile == "" {
		return "", nil, fmt.Errorf("the scaleway backend requires --bootstrap-addr, --bootstrap-tls-cert and --bootstrap-tls-key (the bootstrap blob is a join secret and must travel over TLS)")
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
