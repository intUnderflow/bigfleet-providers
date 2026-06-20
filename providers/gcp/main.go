// Command gcp is the BigFleet CapacityProvider for Google Compute Engine (GCE).
// It implements the substrate-specific providerkit.Backend; providerkit.Server
// wraps it with the full bigfleet.v1alpha1.CapacityProvider contract (fencing,
// idempotency, async dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per GCP region. Production uses the real GCE backend
// (--gcp-backend=gcp, with --project/--region); the in-memory fake backend
// (--gcp-backend=fake, or `auto` with no --region) backs dev and the
// credential-free certification run.
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
		providerLbl = flag.String("provider", "gcp", "provider/region label stamped on HostRefs (e.g. gcp-us-central1)")
		backendSel  = flag.String("gcp-backend", "auto", "gcp | fake | auto (auto = gcp when --region is set, else fake)")
		project     = flag.String("project", "", "GCP project id (required for the gcp backend)")
		region      = flag.String("region", "", "GCP region, e.g. us-central1 (required for the gcp backend)")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		zoneA       = flag.String("zone-a", "", "first zone for default offerings (default: <region>-a)")
		zoneB       = flag.String("zone-b", "", "second zone for default offerings (default: <region>-b)")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		image       = flag.String("image", "projects/debian-cloud/global/images/family/debian-12", "boot disk source image for Instances.Insert (gcp backend)")
		network     = flag.String("network", "global/networks/default", "VPC network for the instance NIC (gcp backend)")
		subnet      = flag.String("subnetwork", "", "subnetwork for the instance NIC, e.g. regions/<r>/subnetworks/<s> (gcp backend; default: the network's auto subnet)")
		diskSizeGB  = flag.Int64("disk-size-gb", 20, "boot disk size in GiB (gcp backend)")
		svcAccount  = flag.String("instance-service-account", "", "service account email the launched instances run as (gcp backend; default: the project default)")
		baseStartup = flag.String("base-startup-script", "", "path to the generic pre-binding startup script baked in at Insert")
		sshKey      = flag.String("ssh-key", "", "path to the SSH private key used for in-band Configure/Drain delivery (gcp backend)")
		sshUser     = flag.String("ssh-user", "bigfleet", "SSH user for Configure/Drain delivery (gcp backend); authorised via ssh-keys metadata")
		bootstrapHk = flag.String("bootstrap-hook", "/opt/bigfleet/bootstrap", "image path that applies the delivered bootstrap blob")
		useExtIP    = flag.Bool("use-external-ip", false, "reach instances over an ephemeral external IP for SSH (gcp backend; default: internal IP, same-VPC)")
		reconcile   = flag.Duration("reconcile-interval", 2*time.Minute, "background GCE->inventory reconcile interval (0 = off)")

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

	// Pick the GCE client.
	mode := resolveBackendMode(*backendSel, *region)
	var client gceClient
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake GCE backend (dev / certification only) — no real instances will be created")
		client = newGCEFake()
	case "gcp":
		var signer ssh.Signer
		if *sshKey != "" {
			s, err := loadSSHSigner(*sshKey)
			if err != nil {
				return err
			}
			signer = s
		} else {
			logger.Warn("no --ssh-key set: Configure cannot deliver the bootstrap blob in-band (Drain will only clear the binding)")
		}
		real, err := newGCEReal(ctx, gceRealConfig{
			Project:                *project,
			Region:                 *region,
			Image:                  *image,
			Network:                *network,
			Subnetwork:             *subnet,
			DiskSizeGB:             *diskSizeGB,
			InstanceServiceAccount: *svcAccount,
			SSHSigner:              signer,
			SSHUser:                *sshUser,
			BootstrapHookPath:      *bootstrapHk,
			UseExternalIP:          *useExtIP,
		}, logger)
		if err != nil {
			return err
		}
		defer func() { _ = real.Close() }()
		client = real
	default:
		return fmt.Errorf("--gcp-backend must be gcp, fake, or auto (got %q)", *backendSel)
	}

	// Instrument every GCE call.
	m := newMetrics()
	client = newMetricsGCEClient(client, m)

	// Offerings.
	var offs []offering
	if *offerings != "" {
		loaded, err := loadOfferings(*offerings)
		if err != nil {
			return err
		}
		offs = loaded
	} else {
		offs = defaultOfferings(*seedCount, defaultZone(*zoneA, *region, "a"), defaultZone(*zoneB, *region, "b"))
	}

	var startupScript []byte
	if *baseStartup != "" {
		b, err := os.ReadFile(*baseStartup)
		if err != nil {
			return fmt.Errorf("read --base-startup-script: %w", err)
		}
		startupScript = b
	}

	pr := newPricing(*region)
	in := newInterruption()
	backend, err := newGCPBackend(*providerLbl, *region, client, offs, pr, in, startupScript, logger)
	if err != nil {
		return err
	}

	// Resolve allocatable (vCPU/memory) for the offered types from the
	// MachineTypes API (best-effort, bounded); the pinned table covers anything
	// GCE can't return. Specs are immutable, so this runs once.
	mtCtx, cancelMT := context.WithTimeout(ctx, 20*time.Second)
	if missed := backend.refreshMachineTypes(mtCtx); missed > 0 {
		logger.Warn("some offered machine types unresolved from GCE; using pinned table", "unresolved", missed)
	}
	cancelMT()

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
			// GCE Instances.Insert + boot + kubelet-ready: minutes, not seconds.
			Create:    5 * time.Minute,
			Configure: 10 * time.Minute, // metadata reset + kubelet join
			Drain:     15 * time.Minute, // strict PDBs can take a while
			Delete:    3 * time.Minute,
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

	// Background loop: GCE->inventory reconcile, which also observes Spot
	// preemptions so preempted slots publish an elevated (observed) interruption
	// probability.
	go runReconciler(ctx, srv, backend, m, *reconcile, logger)

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
		"gcp_backend", mode, "security", securityMode(creds, *tlsCA), "offerings", len(offs),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads GCE truth into kit inventory (new
// offerings, orphans) and, in the same tick, observes Spot preemptions so a
// preempted slot publishes an elevated (observed) interruption probability. The
// persisted store is the primary restart path; this catches drift while running.
func runReconciler(ctx context.Context, srv *providerkit.Server, backend *gcpBackend, m *metrics, interval time.Duration, logger *slog.Logger) {
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

			// Same tick: observe Spot preemptions and raise observed interruption.
			pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
			n, perr := backend.observePreemptions(pctx)
			pcancel()
			switch {
			case perr != nil:
				logger.Warn("observe preemptions failed", "err", perr)
			case n > 0:
				m.preemptions.Add(float64(n))
				logger.Info("observed spot preemptions", "count", n)
			}
		}
	}
}

func resolveBackendMode(flagVal, region string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if region != "" {
			return "gcp"
		}
		return "fake"
	default:
		return strings.ToLower(flagVal)
	}
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

// defaultZone derives a default zone from the region and a suffix, e.g.
// us-central1 + "a" -> us-central1-a.
func defaultZone(override, region, suffix string) string {
	if override != "" {
		return override
	}
	if region != "" {
		return region + "-" + suffix
	}
	return "us-central1-" + suffix
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
