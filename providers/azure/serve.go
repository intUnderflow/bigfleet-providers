package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// buildGRPCServer assembles a production gRPC server: keepalive tuning, a panic-
// recovery + request-logging/metrics interceptor chain, the standard gRPC
// health service, and (optionally) reflection for debugging.
func buildGRPCServer(creds credentials.TransportCredentials, m *metrics, reflect bool, logger *slog.Logger) (*grpc.Server, *health.Server) {
	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 15 * time.Minute,
			Time:              2 * time.Hour,
			Timeout:           20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(recoveryInterceptor(m, logger), loggingInterceptor(m, logger)),
	}
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}
	gs := grpc.NewServer(opts...)
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(gs, hs)
	if reflect {
		reflection.Register(gs)
	}
	return gs, hs
}

// recoveryInterceptor converts a panicking handler into codes.Internal (and a
// metric) instead of crashing the process.
func recoveryInterceptor(m *metrics, logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				if m != nil {
					m.panics.Inc()
				}
				logger.Error("recovered panic in gRPC handler", "method", info.FullMethod, "panic", r)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// loggingInterceptor records per-RPC metrics and a structured log line.
func loggingInterceptor(m *metrics, logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		dur := time.Since(start)
		method := shortMethod(info.FullMethod)
		code := status.Code(err)
		if m != nil {
			m.rpcCalls.WithLabelValues(method, code.String()).Inc()
			m.rpcDuration.WithLabelValues(method).Observe(dur.Seconds())
		}
		lvl := slog.LevelDebug
		switch method {
		case "Create", "Configure", "Drain", "Delete":
			lvl = slog.LevelInfo
		}
		if err != nil {
			lvl = slog.LevelWarn
		}
		logger.Log(ctx, lvl, "rpc", "method", method, "code", code.String(), "dur_ms", dur.Milliseconds())
		return resp, err
	}
}

func shortMethod(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}

// observabilityServer serves /metrics (Prometheus), /healthz (liveness), and
// /readyz (readiness) on a separate HTTP port, so Kubernetes can probe the pod
// and Prometheus can scrape it.
type observabilityServer struct {
	srv   *http.Server
	ready atomic.Bool
}

func newObservabilityServer(addr string, m *metrics) *observabilityServer {
	o := &observabilityServer{}
	mux := http.NewServeMux()
	if m != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if o.ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
	})
	o.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return o
}

func (o *observabilityServer) setReady(b bool) { o.ready.Store(b) }

func (o *observabilityServer) start(logger *slog.Logger) {
	go func() {
		if err := o.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("observability server", "err", err)
		}
	}()
}

func (o *observabilityServer) stop(ctx context.Context) { _ = o.srv.Shutdown(ctx) }
