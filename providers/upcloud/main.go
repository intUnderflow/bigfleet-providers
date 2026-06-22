// Command upcloud is the BigFleet CapacityProvider for UpCloud cloud servers. It
// implements the substrate-specific providerkit.Backend; providerkit.Server
// wraps it with the full bigfleet.v1alpha1.CapacityProvider contract (fencing,
// idempotency, async dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per UpCloud zone. Production uses the real UpCloud backend
// (UPCLOUD_USERNAME / UPCLOUD_PASSWORD of an API sub-account + --zone); the
// in-memory fake backend (--upcloud-backend=fake, or `auto` with no credentials)
// backs dev and the credential-free conformance / certification run.
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
		providerLbl = flag.String("provider", "upcloud", "provider/zone label stamped on HostRefs (e.g. upcloud-fi-hel1)")
		backendSel  = flag.String("upcloud-backend", "auto", "upcloud | fake | auto (auto = upcloud when API credentials AND --zone are set, else fake)")
		username    = flag.String("username", "", "UpCloud API sub-account username (or set UPCLOUD_USERNAME)")
		password    = flag.String("password", "", "UpCloud API sub-account password (or set UPCLOUD_PASSWORD)")
		zone        = flag.String("zone", "", "UpCloud zone id this process serves (e.g. fi-hel1); required for the upcloud backend")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		zoneA       = flag.String("zone-a", "fi-hel1", "first zone for default offerings")
		zoneB       = flag.String("zone-b", "de-fra1", "second zone for default offerings")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		template     = flag.String("template", "", "OS template storage UUID to clone at server create (upcloud backend; e.g. an Ubuntu 24.04 cloud-init template)")
		eurUSD       = flag.Float64("eur-usd", defaultEURtoUSD, "EUR->USD conversion rate applied to the pinned UpCloud price table")
		sshKey       = flag.String("ssh-key", "", "path to the SSH private key used for Configure/Drain delivery (upcloud backend)")
		sshPub       = flag.String("ssh-pubkey", "", "authorized SSH public key injected into servers at create (so --ssh-key can authenticate)")
		sshUser      = flag.String("ssh-user", "root", "SSH user for Configure/Drain delivery (upcloud backend)")
		bootstrapHk  = flag.String("bootstrap-hook", "/opt/bigfleet/bootstrap", "image path that applies the delivered bootstrap blob")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init baked in at server create (installs the on-host hook ONLY — never the bootstrap secret)")
		priceRefresh = flag.Duration("price-refresh", 45*time.Minute, "live price refresh interval (0 = off; the pinned table still seeds startup)")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background UpCloud->inventory reconcile interval (0 = off)")

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

	user := firstNonEmpty(*username, os.Getenv("UPCLOUD_USERNAME"))
	pass := firstNonEmpty(*password, os.Getenv("UPCLOUD_PASSWORD"))

	// Pick the UpCloud client. auto = the real backend when credentials AND a zone
	// are set, otherwise the fake — so a credential-free run (no creds, no zone;
	// exactly how certification boots the binary) defaults to fake.
	mode := resolveBackendMode(*backendSel, user, pass, *zone)
	var client upcloudClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake UpCloud backend (dev / conformance only) — no real servers will be created")
		client = newUpcloudFake()
	case "upcloud":
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
		real, err := newUpcloudReal(upcloudRealConfig{
			Username:          user,
			Password:          pass,
			Zone:              *zone,
			Template:          *template,
			SSHSigner:         signer,
			SSHPublicKey:      *sshPub,
			SSHUser:           *sshUser,
			BootstrapHookPath: *bootstrapHk,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--upcloud-backend must be upcloud, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every UpCloud API call.
	m := newMetrics()
	client = newMetricsUpcloudClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		offs = defaultOfferings(*seedCount, *zoneA, *zoneB)
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
	backend, err := newUpcloudBackend(*providerLbl, *template, client, offs, pr, userData, logger)
	if err != nil {
		return err
	}

	// Resolve allocatable (cores/memory) for the offered plans from the Plans API
	// (best-effort, bounded); the pinned table covers anything UpCloud can't
	// return. Specs are immutable, so this runs once.
	plCtx, cancelPL := context.WithTimeout(ctx, 20*time.Second)
	if missed := backend.refreshPlans(plCtx); missed > 0 {
		logger.Warn("some offered plans unresolved from UpCloud; using pinned table", "unresolved", missed)
	}
	cancelPL()

	// Warm the live price cache before the first List (best-effort, bounded): live
	// UpCloud prices overlay the pinned EUR table, which seeds the cache and is the
	// fallback. Then fail closed — refuse to start if any offered plan would
	// advertise a free (0.0) price, which would corrupt the engine's cost ranking.
	prCtx, cancelPR := context.WithTimeout(ctx, 20*time.Second)
	m.recordPriceRefresh(backend.refreshPrices(prCtx))
	cancelPR()
	if err := backend.requirePrices(); err != nil {
		return err
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
		Timeouts: providerkit.Timeouts{
			// UpCloud server create + boot + kubelet-ready: minutes, not seconds.
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

	// Background loops: live price refresh + UpCloud->inventory reconcile.
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
		"upcloud_backend", mode, "zone", *zone, "security", securityMode(creds, *tlsCA),
		"offerings", len(offs), "metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads UpCloud truth into kit inventory (new
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

// runPriceRefresher periodically refreshes the in-memory price table off the List
// hot path (model: hetzner runPriceRefresher), so List/Describe always read cached
// prices and the cost field tracks the live UpCloud bill rather than a frozen
// snapshot. Each run records its outcome and stamps the last-success gauge for
// staleness monitoring.
func runPriceRefresher(ctx context.Context, backend *upcloudBackend, m *metrics, interval time.Duration) {
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
			m.recordPriceRefresh(failed)
		}
	}
}

// resolveBackendMode picks the substrate client. auto = the real UpCloud backend
// when BOTH API credentials and a zone are set, otherwise the fake — so a
// credential-free run defaults to fake.
func resolveBackendMode(flagVal, user, pass, zone string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if user != "" && pass != "" && zone != "" {
			return "upcloud"
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
