// Command ovhcloud is the BigFleet CapacityProvider for OVHcloud Public Cloud
// (OpenStack-based instances). It implements the substrate-specific
// providerkit.Backend; providerkit.Server wraps it with the full
// bigfleet.v1alpha1.CapacityProvider contract (fencing, idempotency, async
// dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per OVH region. Production uses the real OpenStack backend
// (--region + the OS_* OpenStack-user credentials); the in-memory fake backend
// (--ovh-backend=fake, or `auto` with no --region) backs dev and the
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

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
		providerLbl = flag.String("provider", "ovh-public", "provider/region label stamped on HostRefs (e.g. ovh-public-GRA)")
		backendSel  = flag.String("ovh-backend", "auto", "ovh | fake | auto (auto = ovh when --region is set, else fake)")
		region      = flag.String("region", "", "OVH/OpenStack region (required for the ovh backend, e.g. GRA, SBG, BHS)")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		regionA     = flag.String("region-a", "", "first region for default offerings (default: <region> or GRA)")
		regionB     = flag.String("region-b", "", "second region for default offerings (default: --region when set, else SBG). The real backend rejects offerings outside --region (one process per region), so this only spreads regions for the fake backend.")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		image        = flag.String("image", "", "base image id (UUID) for server create (ovh backend)")
		keyName      = flag.String("key-name", "", "OpenStack keypair name injected for SSH access (ovh backend)")
		network      = flag.String("network", "Ext-Net", "OpenStack network name/id to attach (ovh backend; empty = project default)")
		eurUSD       = flag.Float64("eur-usd", defaultEURtoUSD, "EUR->USD conversion rate applied to OVH prices")
		flavorPrice  = flag.String("flavor-price", "", "comma list of flavor=USD/hour overrides (win over live + seed prices) for flavors the catalog omits or with a negotiated rate (e.g. b2-7=0.03,custom=0.5)")
		priceRefresh = flag.Duration("price-refresh", 45*time.Minute, "live OVH catalog price-refresh interval (0 = off; prices then stay on the dated seed table)")
		priceSub     = flag.String("price-subsidiary", "FR", "OVH subsidiary whose public order catalog supplies live hourly prices; must be a EUR subsidiary (FR, DE, IE, ES, IT, NL, PT, FI, ...) since --eur-usd assumes EUR")
		sshKey       = flag.String("ssh-key", "", "path to the SSH private key used for Configure/Drain delivery (ovh backend)")
		sshUser      = flag.String("ssh-user", "ubuntu", "SSH user for Configure/Drain delivery (ovh backend)")
		bootstrapHk  = flag.String("bootstrap-hook", "/opt/bigfleet/bootstrap", "image path that applies the delivered bootstrap blob")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init baked in at server create")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background OpenStack->inventory reconcile interval (0 = off)")

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

	// Pick the OpenStack client.
	mode := resolveBackendMode(*backendSel, *region)
	var client ovhClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake OVH backend (dev / conformance only) — no real instances will be created")
		client = newOVHFake()
	case "ovh":
		var signer ssh.Signer
		if *sshKey != "" {
			s, err := loadSSHSigner(*sshKey)
			if err != nil {
				return err
			}
			signer = s
		} else {
			logger.Warn("no --ssh-key set: Configure cannot deliver the bootstrap blob over SSH (Drain will only clear the binding metadata)")
		}
		real, err := newOVHReal(ctx, ovhRealConfig{
			Region:            *region,
			Image:             *image,
			KeyName:           *keyName,
			Network:           *network,
			SSHSigner:         signer,
			SSHUser:           *sshUser,
			BootstrapHookPath: *bootstrapHk,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--ovh-backend must be ovh, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every OpenStack API call.
	m := newMetrics()
	client = newMetricsOVHClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		offs = defaultOfferings(*seedCount, defaultRegion(*regionA, *region, "GRA"), defaultRegion(*regionB, *region, "SBG"))
	}

	var userData []byte
	if *baseUserData != "" {
		b, err := os.ReadFile(*baseUserData)
		if err != nil {
			return fmt.Errorf("read --base-user-data: %w", err)
		}
		userData = b
	}

	// Live pricing: the public OVH order catalog (credential-free) supplies hourly
	// flavor prices, refreshed off the List hot path into the pricing cache. Only
	// the real backend wires it — the fake backend stays offline and deterministic
	// (no live calls), so credential-free conformance reproduces, and prices there
	// come from the dated seed table.
	var priceSrc priceSource
	if mode == "ovh" {
		priceSrc = newCatalogPriceSource(ovhCatalogEndpoint, *priceSub, logger, m.observeAPI)
	}
	pr := newPricing(*eurUSD, priceSrc, logger)
	if err := applyPriceOverrides(pr, *flavorPrice); err != nil {
		return err
	}
	backend, err := newOVHBackend(*providerLbl, *region, *image, client, offs, pr, userData, logger)
	if err != nil {
		return err
	}

	// Warm the price cache from the live catalog before the first List (best-effort,
	// bounded). On failure the dated seed table still serves a sane price — the
	// fail-closed startup check guaranteed every offered flavor has one.
	if priceSrc != nil {
		warmCtx, cancelWarm := context.WithTimeout(ctx, 30*time.Second)
		if missing, perr := backend.refreshPrices(warmCtx); perr != nil {
			logger.Warn("pricing: initial live catalog refresh failed; serving the dated seed table (source=manual) until a refresh succeeds", "err", perr)
		} else {
			m.priceLastSuccess.SetToCurrentTime()
			if missing > 0 {
				logger.Warn("pricing: some offered flavors are absent from the OVH catalog; they use the dated seed/override (source=manual)", "missing", missing)
			}
		}
		cancelWarm()
	} else {
		logger.Warn("pricing: no live OVH catalog source (fake backend / dev); prices come from the dated seed table (source=manual)")
	}

	// Resolve allocatable (vCPU/memory) for the offered flavors from the Nova
	// flavors API (best-effort, bounded); the pinned table covers anything
	// OpenStack can't return. Specs are immutable, so this runs once.
	flCtx, cancelFl := context.WithTimeout(ctx, 20*time.Second)
	if missed := backend.refreshFlavors(flCtx); missed > 0 {
		logger.Warn("some offered flavors unresolved from OpenStack; using pinned table", "unresolved", missed)
	}
	cancelFl()

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
			// OpenStack server create + boot + kubelet-ready: minutes, not seconds.
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

	// Background loops: OpenStack->inventory reconcile + live catalog price refresh.
	go runReconciler(ctx, srv, m, *reconcile, logger)
	if priceSrc != nil {
		go runPriceRefresher(ctx, backend, m, *priceRefresh, logger)
	}

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
		"addr", lis.Addr().String(), "provider", *providerLbl, "region", *region,
		"ovh_backend", mode, "security", securityMode(creds, *tlsCA), "offerings", len(offs),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads OpenStack truth into kit inventory (new
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

// runPriceRefresher periodically pulls live hourly prices from the OVH catalog
// into the pricing cache (off the List hot path). A fetch error keeps the prior
// live/seed prices (source=manual) and records an error outcome; a success
// stamps the last-success gauge so staleness is alertable.
func runPriceRefresher(ctx context.Context, backend *ovhBackend, m *metrics, interval time.Duration, logger *slog.Logger) {
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
			rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			_, err := backend.refreshPrices(rctx)
			cancel()
			outcome := "success"
			if err != nil {
				outcome = "error"
				logger.Warn("price refresh failed", "err", err)
			} else {
				m.priceLastSuccess.SetToCurrentTime()
			}
			m.priceRefresh.WithLabelValues(outcome).Inc()
		}
	}
}

// applyPriceOverrides parses a comma list of flavor=USD/hour pairs and registers
// each as a pricing override, so an operator can price a flavor missing from the
// pinned table without a code change (newOVHBackend otherwise rejects it).
func applyPriceOverrides(pr *pricing, spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		flavor, usdStr, ok := strings.Cut(pair, "=")
		flavor = strings.TrimSpace(flavor)
		if !ok || flavor == "" {
			return fmt.Errorf("--flavor-price: expected flavor=USD, got %q", pair)
		}
		usd, err := strconv.ParseFloat(strings.TrimSpace(usdStr), 64)
		// Reject non-positive prices: the flag exists to price a flavor missing
		// from the pinned table, not to model free/sunk-cost capacity. A 0 would
		// publish price_per_hour=0 and always win BigFleet's cost ranking, so an
		// accidental "flavor=0" must fail rather than silently distort scheduling.
		if err != nil || usd <= 0 {
			return fmt.Errorf("--flavor-price: price for %q must be > 0, got %q", flavor, usdStr)
		}
		pr.setOverride(flavor, usd)
	}
	return nil
}

func resolveBackendMode(flagVal, region string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if region != "" {
			return "ovh"
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

// defaultRegion picks the region for a default-offering bucket: the explicit
// override, else the configured --region, else a sensible OVH default.
func defaultRegion(override, region, fallback string) string {
	if override != "" {
		return override
	}
	if region != "" {
		return region
	}
	return fallback
}

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
