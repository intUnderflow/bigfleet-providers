// Command faultprovider is the reference FAULT-INJECTING capacity provider for
// the BigFleet conformance program. It wraps providerkit.Server around a
// minimal in-memory backend that injects substrate faults ON COMMAND — driven
// purely over the wire via the cluster_id and machine labels — so the kit's
// failure/timeout/recovery handling (the B7xx behaviors) is black-box
// certifiable.
//
// It is NOT a copy-me template (that is providers/_template). It exists only to
// give the fault lane of the conformance suite a controllable substrate. The
// fault hooks are documented on faultBackend.
//
// Flags:
//
//	--addr               gRPC listen address (default :9400)
//	--provider           provider/region name stamped on HostRefs (default "fault")
//	--seed-count         number of Speculative slots to seed (default 64)
//	--transition-timeout per-transition timeout for Create/Configure/Drain/Delete
//	                     (default 2s — deliberately SHORT so timeout tests are fast)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func main() {
	if err := run(); err != nil {
		slog.Error("faultprovider exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr              = flag.String("addr", ":9400", "gRPC listen address")
		provider          = flag.String("provider", "fault", "provider/region name stamped on HostRefs")
		seedCount         = flag.Int("seed-count", 64, "number of Speculative slots to seed on boot")
		transitionTimeout = flag.Duration("transition-timeout", defaultTransitionTimeout, "per-transition timeout (short so timeout tests are fast)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	backend := newFaultBackend(*provider, *seedCount, *transitionTimeout)

	srv, err := providerkit.New(backend, providerkit.NewMemStore(), providerkit.Options{
		// The fault lane drives single-zone machines; do not require a zone.
		Logger: logger,
		Timeouts: providerkit.Timeouts{
			Create:    *transitionTimeout,
			Configure: *transitionTimeout,
			Drain:     *transitionTimeout,
			Delete:    *transitionTimeout,
		},
	})
	if err != nil {
		return fmt.Errorf("build faultprovider: %w", err)
	}

	gs := grpc.NewServer()
	srv.Register(gs)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		gs.GracefulStop()
	}()

	logger.Info("serving fault CapacityProvider",
		"addr", lis.Addr().String(), "provider", *provider,
		"seeded", *seedCount, "transition_timeout", transitionTimeout.String())
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
