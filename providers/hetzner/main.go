// Command hetzner is the BigFleet CapacityProvider for Hetzner Cloud. It
// implements the substrate-specific providerkit.Backend; providerkit.Server
// wraps it with the full bigfleet.v1alpha1.CapacityProvider contract (fencing,
// idempotency, async dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per Hetzner location/region. Production uses the real Hetzner
// Cloud backend (--token / HCLOUD_TOKEN); the in-memory fake backend
// (--hetzner-backend=fake, or `auto` with no token) backs dev and the
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
		providerLbl = flag.String("provider", "hetzner", "provider/location label stamped on HostRefs (e.g. hetzner-nbg1)")
		backendSel  = flag.String("hetzner-backend", "auto", "hetzner | fake | auto (auto = hetzner when a token is set; else refuses to start unless --use-fake-backend is passed)")
		useFake     = flag.Bool("use-fake-backend", false, "run the credential-free in-memory fake backend (testing/conformance only — it never creates real cloud resources)")
		token       = flag.String("token", "", "Hetzner Cloud API token (or set HCLOUD_TOKEN)")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		locA        = flag.String("location-a", "nbg1", "first location for default offerings")
		locB        = flag.String("location-b", "fsn1", "second location for default offerings")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		image        = flag.String("image", "", "base image name/id for Server.Create (hetzner backend)")
		eurUSD       = flag.Float64("eur-usd", defaultEURtoUSD, "EUR->USD conversion rate applied to Hetzner prices")
		sshKey       = flag.String("ssh-key", "", "path to the SSH private key used for Configure/Drain delivery (hetzner backend)")
		sshUser      = flag.String("ssh-user", "root", "SSH user for Configure/Drain delivery (hetzner backend)")
		bootstrapHk  = flag.String("bootstrap-hook", "/opt/bigfleet/bootstrap", "image path that applies the delivered bootstrap blob")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init baked in at server create")
		priceRefresh = flag.Duration("price-refresh", 30*time.Minute, "price refresh interval (0 = off)")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background Hetzner->inventory reconcile interval (0 = off)")

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

	hetznerToken := *token
	if hetznerToken == "" {
		hetznerToken = os.Getenv("HCLOUD_TOKEN")
	}

	// Pick the Hetzner Cloud client.
	mode := resolveBackendMode(*backendSel, hetznerToken)
	if *useFake {
		mode = "fake"
	} else if mode == "fake" && strings.ToLower(*backendSel) != "fake" {
		return fmt.Errorf("refusing to start the hetzner provider on the in-memory fake backend: no credentials were detected. Configure the real backend, or pass --use-fake-backend to run the credential-free fake (testing/conformance only — it never creates real resources)")
	}
	var client hcloudClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake Hetzner backend (dev / conformance only) — no real servers will be created")
		client = newHCloudFake()
	case "hetzner":
		var signer ssh.Signer
		if *sshKey != "" {
			s, err := loadSSHSigner(*sshKey)
			if err != nil {
				return err
			}
			signer = s
		} else {
			logger.Warn("no --ssh-key set: Configure cannot deliver the bootstrap blob over SSH (Drain will only clear the binding label)")
		}
		real, err := newHCloudReal(hcloudRealConfig{
			Token:             hetznerToken,
			Image:             *image,
			Location:          *locA,
			EURtoUSD:          *eurUSD,
			SSHSigner:         signer,
			SSHUser:           *sshUser,
			BootstrapHookPath: *bootstrapHk,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--hetzner-backend must be hetzner, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every Hetzner API call.
	m := newMetrics()
	client = newMetricsHCloudClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		offs = defaultOfferings(*seedCount, *locA, *locB)
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
	backend, err := newHetznerBackend(*providerLbl, *image, client, offs, pr, userData, logger)
	if err != nil {
		return err
	}

	// Warm the price cache before first List (best-effort, bounded).
	warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Second)
	backend.refreshPrices(warmCtx)
	cancelWarm()

	// Resolve allocatable (vCPU/memory) for the offered types from the ServerType
	// API (best-effort, bounded); the pinned table covers anything Hetzner can't
	// return. Specs are immutable, so this runs once.
	stCtx, cancelST := context.WithTimeout(ctx, 20*time.Second)
	if missed := backend.refreshServerTypes(stCtx); missed > 0 {
		logger.Warn("some offered server types unresolved from Hetzner; using pinned table", "unresolved", missed)
	}
	cancelST()

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv, err := providerkit.New(backend, store, providerkit.Options{
		// Multi-location provider: require a zone (location) on every machine.
		RequireZone: true,
		Logger:      logger,
		Timeouts: providerkit.Timeouts{
			// Hetzner Server.Create + boot + kubelet-ready: minutes, not seconds.
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

	// Background loops: price refresh + Hetzner->inventory reconcile.
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
		gs.GracefulStop()
	}()

	logger.Info("serving CapacityProvider",
		"addr", lis.Addr().String(), "provider", *providerLbl,
		"hetzner_backend", mode, "security", securityMode(creds, *tlsCA), "offerings", len(offs),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads Hetzner truth into kit inventory (new
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

func runPriceRefresher(ctx context.Context, backend *hetznerBackend, m *metrics, interval time.Duration) {
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

func resolveBackendMode(flagVal, token string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if token != "" {
			return "hetzner"
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
