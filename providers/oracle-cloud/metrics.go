package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the OCI provider's Prometheus instrumentation, exposed at /metrics.
// It matches BigFleet's stack (prometheus/client_golang) so an operator gets OCI
// Compute API visibility, RPC latency/outcomes, and the background loops' health
// out of the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls    *prometheus.CounterVec   // bigfleet_oci_api_calls_total{op,outcome}
	apiDuration *prometheus.HistogramVec // bigfleet_oci_api_duration_seconds{op}
	rpcCalls    *prometheus.CounterVec   // bigfleet_oci_grpc_requests_total{method,code}
	rpcDuration *prometheus.HistogramVec // bigfleet_oci_grpc_request_duration_seconds{method}
	panics      prometheus.Counter       // bigfleet_oci_panics_total
	reconcile   *prometheus.CounterVec   // bigfleet_oci_reconcile_total{outcome}
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_oci_api_calls_total",
			Help: "OCI Compute API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_oci_api_duration_seconds",
			Help:    "OCI Compute API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_oci_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_oci_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_oci_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_oci_reconcile_total",
			Help: "Background reconcile runs by outcome.",
		}, "outcome"),
	}
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *metrics) observeAPI(op string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.apiCalls.WithLabelValues(op, outcome).Inc()
	m.apiDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// metricsOCIClient decorates an ociClient, recording call counts/latency per
// operation. It is transparent — it returns exactly what the inner client does.
type metricsOCIClient struct {
	inner ociClient
	m     *metrics
}

func newMetricsOCIClient(inner ociClient, m *metrics) ociClient {
	if m == nil {
		return inner
	}
	return &metricsOCIClient{inner: inner, m: m}
}

func (c *metricsOCIClient) LaunchInstance(ctx context.Context, spec launchSpec) (ociInstance, error) {
	start := time.Now()
	inst, err := c.inner.LaunchInstance(ctx, spec)
	c.m.observeAPI("LaunchInstance", start, err)
	return inst, err
}
func (c *metricsOCIClient) TerminateInstance(ctx context.Context, id string) error {
	start := time.Now()
	err := c.inner.TerminateInstance(ctx, id)
	c.m.observeAPI("TerminateInstance", start, err)
	return err
}
func (c *metricsOCIClient) DescribeManaged(ctx context.Context) ([]ociInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("DescribeManaged", start, err)
	return out, err
}
func (c *metricsOCIClient) ApplyBootstrap(ctx context.Context, inst ociInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, inst, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsOCIClient) DrainNode(ctx context.Context, inst ociInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, inst, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}

var _ ociClient = (*metricsOCIClient)(nil)

// promauto is a tiny factory that registers each metric on a specific registry
// (so the provider uses an isolated registry, not the global default).
type promFactory struct{ reg *prometheus.Registry }

func promauto(reg *prometheus.Registry) promFactory { return promFactory{reg} }

func (f promFactory) counter(o prometheus.CounterOpts) prometheus.Counter {
	c := prometheus.NewCounter(o)
	f.reg.MustRegister(c)
	return c
}
func (f promFactory) counterVec(o prometheus.CounterOpts, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(o, labels)
	f.reg.MustRegister(c)
	return c
}
func (f promFactory) histogramVec(o prometheus.HistogramOpts, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(o, labels)
	f.reg.MustRegister(h)
	return h
}
