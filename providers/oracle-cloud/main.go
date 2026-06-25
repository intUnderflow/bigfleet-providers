// Command oracle-cloud is the BigFleet CapacityProvider for Oracle Cloud
// Infrastructure (OCI) Compute. It implements the substrate-specific
// providerkit.Backend; providerkit.Server wraps it with the full
// bigfleet.v1alpha1.CapacityProvider contract (fencing, idempotency, async
// dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per OCI region/compartment. Production uses the real OCI Compute
// backend (--region + --compartment + an OCI auth mode); the in-memory fake
// backend (--oci-backend=fake, or `auto` with no region/compartment) backs dev
// and the credential-free conformance/certification run.
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
		providerLbl = flag.String("provider", "oci", "provider/region label stamped on HostRefs (e.g. oci-eu-frankfurt-1)")
		backendSel  = flag.String("oci-backend", "auto", "oci | fake | auto (auto = oci when --region and --compartment are set; else refuses to start unless --use-fake-backend is passed)")
		useFake     = flag.Bool("use-fake-backend", false, "run the credential-free in-memory fake backend (testing/conformance only — it never creates real cloud resources)")
		region      = flag.String("region", "", "OCI region identifier, e.g. eu-frankfurt-1 (required for the oci backend)")
		compartment = flag.String("compartment", "", "compartment OCID the provider operates in (oci backend)")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		adA         = flag.String("ad-a", "", "first availability domain for default offerings (default: <region>-AD-1)")
		adB         = flag.String("ad-b", "", "second availability domain for default offerings (default: <region>-AD-2)")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		authMode     = flag.String("auth", "auto", "OCI auth: instance_principal | workload_identity | config_file | auto")
		subnet       = flag.String("subnet", "", "subnet OCID for LaunchInstance (oci backend)")
		image        = flag.String("image", "", "base image OCID for LaunchInstance (oci backend)")
		pricesFile   = flag.String("prices-file", "", "path to a price table YAML used as the startup seed + fallback (default: the embedded prices.yaml)")
		priceListURL = flag.String("price-list-url", "", "OCI price-list API URL for live price refresh (default: the public cost-estimator endpoint)")
		priceRefresh = flag.Duration("price-refresh", 45*time.Minute, "live price refresh interval (0 = off; seed/fallback prices only)")
		bootstrapHk  = flag.String("bootstrap-hook", "/opt/bigfleet/bootstrap", "image path that applies the delivered bootstrap blob")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init baked in at launch")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background OCI->inventory reconcile interval (0 = off)")

		preemptStream = flag.String("preemption-stream", "", "OCID of an OCI Streaming stream fed by an Events rule (event type com.oraclecloud.computeapi.instancepreemptionaction); when set, observed preemptions raise interruption_probability for the affected SPOT machine")

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

	// Pick the OCI Compute client.
	mode := resolveBackendMode(*backendSel, *region, *compartment)
	if *useFake {
		mode = "fake"
	} else if mode == "fake" && strings.ToLower(*backendSel) != "fake" {
		return fmt.Errorf("refusing to start the oracle-cloud provider on the in-memory fake backend: no credentials were detected. Configure the real backend, or pass --use-fake-backend to run the credential-free fake (testing/conformance only — it never creates real resources)")
	}
	var client ociClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake OCI backend (dev / conformance only) — no real instances will be created")
		client = newOCIFake()
	case "oci":
		real, err := newOCIReal(ctx, ociRealConfig{
			Region:            *region,
			CompartmentOCID:   *compartment,
			SubnetOCID:        *subnet,
			ImageOCID:         *image,
			AuthMode:          *authMode,
			BootstrapHookPath: *bootstrapHk,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--oci-backend must be oci, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every OCI Compute API call.
	m := newMetrics()
	client = newMetricsOCIClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		offs = defaultOfferings(*seedCount, defaultAD(*adA, *region, 1), defaultAD(*adB, *region, 2))
	}

	var userData []byte
	if *baseUserData != "" {
		b, err := os.ReadFile(*baseUserData)
		if err != nil {
			return fmt.Errorf("read --base-user-data: %w", err)
		}
		userData = b
	}

	// Live price source: the fake backend (dev / credential-free certify) uses a
	// deterministic, network-free source; the oci backend reads the public OCI
	// price list over HTTP (no credentials). prices.yaml is the startup seed +
	// fallback for both.
	var priceSrc priceSource
	if mode == "fake" {
		priceSrc = newFakePriceSource()
	} else {
		priceSrc = newOCIPriceList(*priceListURL, logger)
	}
	pr, err := newPricing(*pricesFile, priceSrc, logger)
	if err != nil {
		return err
	}
	in := newInterruption()
	backend, err := newOCIBackend(*providerLbl, client, offs, pr, in, userData, logger)
	if err != nil {
		return err
	}

	// Warm the price tables from the live source before serving (best-effort,
	// bounded): on success List/Describe emit live prices from the first request;
	// on failure they fall back to the prices.yaml seed.
	warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Second)
	if err := backend.refreshPrices(warmCtx); err != nil {
		logger.Warn("initial price refresh failed; using prices.yaml seed/fallback", "err", err)
	} else {
		m.priceLastSuccess.SetToCurrentTime()
	}
	cancelWarm()

	// Fail closed: refuse to start if any hourly-billed offering would emit a
	// price of 0 (it would rank as free). A bare_metal lane's 0 is honest.
	if err := backend.validatePricing(); err != nil {
		return err
	}

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv, err := providerkit.New(backend, store, providerkit.Options{
		// Multi-AD provider: require a zone (availability domain) on every machine.
		RequireZone: true,
		Logger:      logger,
		Timeouts: providerkit.Timeouts{
			// OCI LaunchInstance + boot + kubelet-ready: minutes, not seconds.
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

	// Background loops: live price refresh + OCI->inventory reconcile.
	go runPriceRefresher(ctx, backend, m, *priceRefresh, logger)
	go runReconciler(ctx, srv, m, *reconcile, logger)

	// Live preemption observer: when an OCI Streaming stream (fed by an Events rule
	// for preemption-action events) is configured, consume it and raise the
	// observed interruption probability of a SPOT machine about to be reclaimed.
	// Real backend only — the fake has no live OCI events.
	if mode == "oci" && *preemptStream != "" {
		poller, err := newPreemptionPoller(ctx, *authMode, *region, *preemptStream, backend, m, logger)
		if err != nil {
			return err
		}
		go poller.run(ctx)
		logger.Info("watching for preemptions", "stream", *preemptStream)
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
		"oci_backend", mode, "security", securityMode(creds, *tlsCA), "offerings", len(offs),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads OCI truth into kit inventory (new
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

// runPriceRefresher periodically pulls the live OCI price list into the in-memory
// price tables, off the List hot path (model: the hetzner provider). It records
// success/failure and the last-success timestamp so an operator can alert on a
// stale price table (staleness = time() - the timestamp gauge).
func runPriceRefresher(ctx context.Context, backend *ociBackend, m *metrics, interval time.Duration, logger *slog.Logger) {
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
			err := backend.refreshPrices(rctx)
			cancel()
			outcome := "success"
			if err != nil {
				outcome = "error"
				logger.Warn("price refresh failed; keeping previous prices",
					"err", err, "staleness_seconds", backend.pricing.stalenessSeconds())
			} else {
				m.priceLastSuccess.SetToCurrentTime()
			}
			m.priceRefresh.WithLabelValues(outcome).Inc()
		}
	}
}

// resolveBackendMode picks the backend: explicit oci/fake, or auto (oci only when
// both a region and a compartment are configured, else fake). Certify boots with
// neither, so auto resolves to the credential-free fake — load-bearing for the
// credential-free certification gate.
func resolveBackendMode(flagVal, region, compartment string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if region != "" && compartment != "" {
			return "oci"
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

// defaultAD synthesises an availability-domain name for the default offerings
// when none is supplied. OCI AD names are tenancy-prefixed
// (e.g. "Uocm:EU-FRANKFURT-1-AD-1"), so a real deployment must pass --ad-a/--ad-b
// or an --offerings file; this is only a placeholder for dev/conformance.
func defaultAD(override, region string, n int) string {
	if override != "" {
		return override
	}
	r := region
	if r == "" {
		r = "phx"
	}
	return fmt.Sprintf("%s-AD-%d", strings.ToUpper(r), n)
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
