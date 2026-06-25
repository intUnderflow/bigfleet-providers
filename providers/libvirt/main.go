// Command libvirt is the BigFleet CapacityProvider for libvirt (QEMU/KVM VMs).
// It implements the substrate-specific providerkit.Backend; providerkit.Server
// wraps it with the full bigfleet.v1alpha1.CapacityProvider contract (fencing,
// idempotency, async dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per libvirt deployment (one or more hosts, one zone per host).
// Production uses the real go-libvirt backend (--connect qemu:///system or a
// zone=uri list); the in-memory fake backend (--libvirt-backend=fake, or `auto`
// with no --connect) backs dev and the credential-free certification run.
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
		providerLbl = flag.String("provider", "libvirt", "provider label stamped on HostRefs (e.g. libvirt-rack1)")
		backendSel  = flag.String("libvirt-backend", "auto", "libvirt | fake | auto (auto = libvirt when --connect is set; else refuses to start unless --use-fake-backend is passed)")
		useFake     = flag.Bool("use-fake-backend", false, "run the credential-free in-memory fake backend (testing/conformance only — it never creates real cloud resources)")
		connect     = flag.String("connect", "", "libvirt connection: a bare URI (qemu:///system) for the default zone, or a comma-separated zone=uri list for multi-host")
		defaultZone = flag.String("default-zone", "local", "zone label for a single bare --connect URI (and the fake backend's hosts)")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		capacity    = flag.String("capacity-type", "on_demand", "capacity_type for the default offerings: on_demand (Delete implemented) or bare_metal (fixed free pool)")

		typesFile = flag.String("instance-types", "", "path to a JSON instance-type catalog (name -> {vcpu, memory_mib}); default: built-in kvm.* sizes")
		prices    = flag.String("prices", "", "explicit USD/hour per instance type as type=usd pairs (default: synthetic per-vCPU/GiB pricing)")
		perVCPU   = flag.Float64("price-per-vcpu-hour", defaultPerVCPUHour, "synthetic USD/hour per vCPU when no explicit price is set")
		perGiB    = flag.Float64("price-per-gib-hour", defaultPerGiBHour, "synthetic USD/hour per GiB RAM when no explicit price is set")

		image        = flag.String("image", "", "base/golden cloud image volume name the overlay disk backs onto (libvirt backend)")
		storagePool  = flag.String("storage-pool", "default", "libvirt storage pool for overlay + cloud-init volumes (libvirt backend)")
		network      = flag.String("network", "default", "libvirt network the domain NIC attaches to (libvirt backend)")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding cloud-init user-data baked in at domain define")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background libvirt->inventory reconcile interval (0 = off)")

		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")
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

	// Resolve the libvirt host connections (zone -> URI).
	conns, err := parseConnections(*connect, *defaultZone)
	if err != nil {
		return err
	}

	// Instance-type catalog.
	typeMap, err := loadInstanceTypes(*typesFile)
	if err != nil {
		return err
	}
	catalog := newInstanceCatalog(typeMap)

	priceOverrides, err := parsePriceOverrides(*prices)
	if err != nil {
		return err
	}
	pr := newPricing(catalog, *perVCPU, *perGiB, priceOverrides)

	// Pick the libvirt client.
	mode := resolveBackendMode(*backendSel, conns)
	if *useFake {
		mode = "fake"
	} else if mode == "fake" && strings.ToLower(*backendSel) != "fake" {
		return fmt.Errorf("refusing to start the libvirt provider on the in-memory fake backend: no credentials were detected. Configure the real backend, or pass --use-fake-backend to run the credential-free fake (testing/conformance only — it never creates real resources)")
	}
	var client libvirtClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake libvirt backend (dev / certification only) — no real domains will be created")
		client = newLibvirtFake()
	case "libvirt":
		real, err := newLibvirtReal(libvirtRealConfig{
			Connections: conns,
			Image:       *image,
			StoragePool: *storagePool,
			Network:     *network,
		}, logger)
		if err != nil {
			return err
		}
		client = real
	default:
		return fmt.Errorf("--libvirt-backend must be libvirt, fake, or auto (got %q)", *backendSel)
	}
	defer func() { _ = client.Close() }()

	// Instrument every libvirt API call.
	m := newMetrics()
	client = newMetricsLibvirtClient(client, m)

	// Offerings: the hosts (zones) the default offerings spread across come from
	// the configured connections (fake or real); fall back to the default zone.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		hostA, hostB := defaultZones(conns, *defaultZone)
		offs = defaultOfferings(*seedCount, hostA, hostB, *capacity, catalog.names())
	}

	// With the real backend, every offering must place onto a configured --connect
	// zone, or Create would only fail at runtime ("no libvirt connection for zone")
	// after the provider is already serving. Fail fast at startup instead.
	if mode == "libvirt" {
		if err := validateOfferingZones(offs, conns); err != nil {
			return err
		}
	}

	var userData []byte
	if *baseUserData != "" {
		b, err := os.ReadFile(*baseUserData)
		if err != nil {
			return fmt.Errorf("read --base-user-data: %w", err)
		}
		userData = b
	}

	backend, err := newLibvirtBackend(*providerLbl, client, offs, catalog, pr, userData, logger)
	if err != nil {
		return err
	}

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// A pure bare-metal pool registers without Deleter (Delete -> Unimplemented);
	// any on-demand capacity keeps Delete.
	srv, err := providerkit.New(selectBackend(backend), store, providerkit.Options{
		// Multi-host provider: require a zone (libvirt host) on every machine.
		RequireZone: true,
		Logger:      logger,
		Timeouts: providerkit.Timeouts{
			// Domain define + boot + cloud-init first boot: tens of seconds to
			// minutes, not a request timeout.
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

	// Background loop: libvirt->inventory reconcile (the persisted store is the
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
		"libvirt_backend", mode, "security", securityMode(creds, *tlsCA),
		"offerings", len(offs), "zones", strings.Join(zonesOf(conns), ","),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads libvirt truth into kit inventory (new
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

// resolveBackendMode picks the libvirt client: auto resolves to the real backend
// when at least one --connect host is configured, else the fake. This is what
// makes the credential-free certification run (which boots the binary with no
// --connect) default to the fake, while a real deployment that sets --connect
// gets the real backend.
// validateOfferingZones rejects an offering placed on a zone that has no
// configured --connect host, so a typo fails fast at startup rather than as a
// runtime Create error after the provider is already serving.
func validateOfferingZones(offs []offering, conns []hostConn) error {
	zones := make(map[string]bool, len(conns))
	for _, c := range conns {
		zones[c.Zone] = true
	}
	for _, off := range offs {
		if !zones[off.Zone] {
			return fmt.Errorf("offering %s is placed on zone %q, which has no --connect host (configured zones: %s)",
				off.InstanceType, off.Zone, strings.Join(zonesOf(conns), ", "))
		}
	}
	return nil
}

func resolveBackendMode(flagVal string, conns []hostConn) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if len(conns) > 0 {
			return "libvirt"
		}
		return "fake"
	default:
		return strings.ToLower(flagVal)
	}
}

// defaultZones returns two host/zone names to spread the default offerings
// across. With configured connections it uses the first two (deduped to one if
// only a single host); with none (the fake backend) it synthesises two zones
// around the default zone so a conformance run exercises multi-zone placement.
func defaultZones(conns []hostConn, defaultZone string) (string, string) {
	switch {
	case len(conns) >= 2:
		return conns[0].Zone, conns[1].Zone
	case len(conns) == 1:
		return conns[0].Zone, conns[0].Zone
	default:
		return defaultZone + "-a", defaultZone + "-b"
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
