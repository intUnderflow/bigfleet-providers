// Command aws is the BigFleet CapacityProvider for AWS EC2. It implements the
// substrate-specific providerkit.Backend; providerkit.Server wraps it with the
// full bigfleet.v1alpha1.CapacityProvider contract (fencing, idempotency,
// async dispatch, transition timeouts, shard_metadata, field-shape).
//
// One process per (AWS region). Production uses the real EC2 backend; the
// in-memory fake backend (`--ec2-backend=fake`, or `auto` with no --region)
// backs dev and the credential-free conformance run.
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
		providerLbl = flag.String("provider", "aws", "provider/region label stamped on HostRefs (e.g. aws-us-east-1)")
		region      = flag.String("region", "", "AWS region (required for the aws backend)")
		ec2Backend  = flag.String("ec2-backend", "auto", "aws | fake | auto (auto = aws when --region is set, else fake)")
		offerings   = flag.String("offerings", "", "path to a JSON offerings file (default: a built-in mix sized by --seed-count)")
		seedCount   = flag.Int("seed-count", 32, "number of Speculative slots when using the default offerings")
		zoneA       = flag.String("zone-a", "", "first AZ for default offerings (default: <region>a)")
		zoneB       = flag.String("zone-b", "", "second AZ for default offerings (default: <region>b)")
		statePath   = flag.String("state", "", "durable state file (empty = in-memory only)")

		ami          = flag.String("ami", "", "base AMI id for RunInstances (aws backend)")
		subnets      = flag.String("subnets", "", "comma list of zone=subnet-id (aws backend)")
		secGroups    = flag.String("security-groups", "", "comma list of security group ids (aws backend)")
		iamProfile   = flag.String("iam-instance-profile", "", "instance profile name granting SSM (aws backend)")
		keyName      = flag.String("key-name", "", "optional EC2 SSH key name")
		bootstrapHk  = flag.String("bootstrap-hook", "/opt/bigfleet/bootstrap", "AMI path that applies the delivered bootstrap blob")
		baseUserData = flag.String("base-user-data", "", "path to the generic pre-binding bootstrap baked in at launch")
		spotRefresh  = flag.Duration("spot-refresh", 5*time.Minute, "spot price refresh interval")
		odRefresh    = flag.Duration("ondemand-refresh", 60*time.Minute, "on-demand price refresh interval from the public AWS Price List Bulk API (0 = off, seed/fallback table only)")
		spotIntrQ    = flag.String("spot-interruption-queue", "", "SQS queue URL fed by an EventBridge spot-interruption/rebalance rule (aws backend; raises observed interruption probability)")
		reconcile    = flag.Duration("reconcile-interval", 2*time.Minute, "background EC2->inventory reconcile interval (0 = off)")

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

	// Pick the EC2 client.
	mode := resolveBackendMode(*ec2Backend, *region)
	var ec2c ec2Client
	switch mode {
	case "fake":
		logger.Warn("using the IN-MEMORY fake EC2 backend (dev / conformance only) — no real instances will be created")
		ec2c = newEC2Fake()
	case "aws":
		subnetMap, err := parseKV(*subnets)
		if err != nil {
			return fmt.Errorf("--subnets: %w", err)
		}
		real, err := newEC2Real(ctx, ec2RealConfig{
			Region:             *region,
			AMI:                *ami,
			Subnets:            subnetMap,
			SecurityGroupIDs:   splitComma(*secGroups),
			IAMInstanceProfile: *iamProfile,
			KeyName:            *keyName,
			BootstrapHookPath:  *bootstrapHk,
		}, logger)
		if err != nil {
			return err
		}
		ec2c = real
	default:
		return fmt.Errorf("--ec2-backend must be aws, fake, or auto (got %q)", *ec2Backend)
	}

	// Instrument every EC2/SSM call.
	m := newMetrics()
	ec2c = newMetricsEC2Client(ec2c, m)

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

	var userData []byte
	if *baseUserData != "" {
		b, err := os.ReadFile(*baseUserData)
		if err != nil {
			return fmt.Errorf("read --base-user-data: %w", err)
		}
		userData = b
	}

	pr := newPricing(*region, ec2c, logger)
	in := newInterruption()
	backend, err := newAWSBackend(*providerLbl, *region, ec2c, offs, pr, in, userData, logger)
	if err != nil {
		return err
	}

	// Warm the spot price cache before first List (best-effort, bounded).
	warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Second)
	backend.refreshPrices(warmCtx)
	cancelWarm()

	// Warm the live on-demand price cache before first List. The offer file is
	// large, so allow a longer (still bounded) budget; on timeout/error the
	// pinned seed table backstops every price, so this stays best-effort.
	odWarmCtx, cancelODWarm := context.WithTimeout(ctx, 90*time.Second)
	recordOnDemandRefresh(m, backend.refreshOnDemandPrices(odWarmCtx))
	cancelODWarm()

	// Fail closed: an on-demand / reserved offering whose type has neither a
	// live price nor a pinned seed would emit price_per_hour=0, which wins the
	// shard's cost ranking. Reject at startup rather than silently mis-rank.
	if bad := backend.unpricedOnDemand(); len(bad) > 0 {
		return fmt.Errorf("on-demand pricing: instance types have no live or pinned price (would emit price_per_hour=0, winning the cost signal): %s — add them to onDemandByRegion (see cmd/genpricing) or remove them from offerings", strings.Join(bad, ", "))
	}

	// Resolve allocatable (vCPU/memory) for the offered types from
	// DescribeInstanceTypes (best-effort, bounded); the pinned table covers
	// anything AWS can't return. Specs are immutable, so this runs once.
	itCtx, cancelIT := context.WithTimeout(ctx, 20*time.Second)
	if missed := backend.refreshInstanceTypes(itCtx); missed > 0 {
		logger.Warn("some offered instance types unresolved from AWS; using pinned table", "unresolved", missed)
	}
	cancelIT()

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
			// EC2 RunInstances + boot + kubelet-ready: minutes, not seconds.
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

	// The Spot interruption advisor buckets (interruption.go) are still pinned
	// us-east-1 approximations; warn when serving another region. On-demand
	// pricing is region-keyed (newPricing logs its own per-region fallback) and
	// allocatable is resolved live from DescribeInstanceTypes, so only the
	// interruption buckets need a region caveat here.
	if mode == "aws" && *region != "" && *region != "us-east-1" {
		logger.Warn("spot interruption-probability buckets are us-east-1 approximations; verify advisorBucket for this region", "region", *region)
	}

	// Background loops: spot + on-demand price refresh + EC2->inventory reconcile.
	go runSpotRefresher(ctx, backend, m, *spotRefresh)
	go runOnDemandRefresher(ctx, backend, m, *odRefresh, logger)
	go runReconciler(ctx, srv, m, *reconcile, logger)

	// Observed spot interruptions (optional): an SQS queue fed by EventBridge.
	if mode == "aws" && *spotIntrQ != "" {
		poller, err := newInterruptionPoller(ctx, *region, *spotIntrQ, backend, m, logger)
		if err != nil {
			return err
		}
		go poller.run(ctx)
		logger.Info("watching for spot interruptions", "queue", *spotIntrQ)
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
		"ec2_backend", mode, "security", securityMode(creds, *tlsCA), "offerings", len(offs),
		"metrics_addr", *metricsAddr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// runReconciler periodically re-reads EC2 truth into kit inventory (new
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

func resolveBackendMode(flagVal, region string) string {
	switch strings.ToLower(flagVal) {
	case "auto", "":
		if region != "" {
			return "aws"
		}
		return "fake"
	default:
		return strings.ToLower(flagVal)
	}
}

func runSpotRefresher(ctx context.Context, backend *awsBackend, m *metrics, interval time.Duration) {
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
			m.spotRefresh.WithLabelValues(outcome).Inc()
		}
	}
}

// runOnDemandRefresher periodically pulls live on-demand prices from the public
// AWS Price List Bulk API into the in-memory cache, off the List hot path. The
// pinned seed table backstops any failed/missing price, so a refresh error
// degrades freshness, not correctness — it logs the staleness and serves the
// last-known prices.
func runOnDemandRefresher(ctx context.Context, backend *awsBackend, m *metrics, interval time.Duration, logger *slog.Logger) {
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
			// The offer file is tens of MB; allow a longer (bounded) budget.
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			failed := backend.refreshOnDemandPrices(rctx)
			cancel()
			recordOnDemandRefresh(m, failed)
			if failed > 0 {
				logger.Warn("on-demand price refresh failed; serving last-known on-demand prices from cache/seed")
			}
		}
	}
}

// recordOnDemandRefresh updates the on-demand refresh metrics: the success/error
// counter and, on success, the last-successful-refresh timestamp gauge (from
// which an operator computes staleness as now - gauge).
func recordOnDemandRefresh(m *metrics, failed int) {
	if m == nil {
		return
	}
	if failed > 0 {
		m.onDemandRefresh.WithLabelValues("error").Inc()
		return
	}
	m.onDemandRefresh.WithLabelValues("success").Inc()
	m.onDemandLastSuccess.SetToCurrentTime()
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

func defaultZone(override, region, suffix string) string {
	if override != "" {
		return override
	}
	if region != "" {
		return region + suffix
	}
	return "us-east-1" + suffix
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseKV(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("expected zone=subnet, got %q", pair)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
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
