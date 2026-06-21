package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the OVHcloud provider's Prometheus instrumentation, exposed at
// /metrics. It matches BigFleet's stack (prometheus/client_golang) so an
// operator gets OpenStack API visibility, RPC latency/outcomes, and the
// background reconcile loop's health out of the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls    *prometheus.CounterVec   // bigfleet_ovh_api_calls_total{op,outcome}
	apiDuration *prometheus.HistogramVec // bigfleet_ovh_api_duration_seconds{op}
	rpcCalls    *prometheus.CounterVec   // bigfleet_ovh_grpc_requests_total{method,code}
	rpcDuration *prometheus.HistogramVec // bigfleet_ovh_grpc_request_duration_seconds{method}
	panics      prometheus.Counter       // bigfleet_ovh_panics_total
	reconcile   *prometheus.CounterVec   // bigfleet_ovh_reconcile_total{outcome}
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_ovh_api_calls_total",
			Help: "OpenStack API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_ovh_api_duration_seconds",
			Help:    "OpenStack API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_ovh_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_ovh_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_ovh_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_ovh_reconcile_total",
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

// metricsOVHClient decorates an ovhClient, recording call counts/latency per
// operation. It is transparent — it returns exactly what the inner client does.
type metricsOVHClient struct {
	inner ovhClient
	m     *metrics
}

func newMetricsOVHClient(inner ovhClient, m *metrics) ovhClient {
	if m == nil {
		return inner
	}
	return &metricsOVHClient{inner: inner, m: m}
}

func (c *metricsOVHClient) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	start := time.Now()
	srv, err := c.inner.CreateServer(ctx, spec)
	c.m.observeAPI("CreateServer", start, err)
	return srv, err
}
func (c *metricsOVHClient) DeleteServer(ctx context.Context, id string) error {
	start := time.Now()
	err := c.inner.DeleteServer(ctx, id)
	c.m.observeAPI("DeleteServer", start, err)
	return err
}
func (c *metricsOVHClient) StartServer(ctx context.Context, id string) error {
	start := time.Now()
	err := c.inner.StartServer(ctx, id)
	c.m.observeAPI("StartServer", start, err)
	return err
}
func (c *metricsOVHClient) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("DescribeManaged", start, err)
	return out, err
}
func (c *metricsOVHClient) ApplyBootstrap(ctx context.Context, srv serverInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, srv, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsOVHClient) DrainNode(ctx context.Context, srv serverInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, srv, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}
func (c *metricsOVHClient) DescribeFlavorCapacities(ctx context.Context, flavors []string) (map[string]flavorCapacity, error) {
	start := time.Now()
	out, err := c.inner.DescribeFlavorCapacities(ctx, flavors)
	c.m.observeAPI("Flavors", start, err)
	return out, err
}

var _ ovhClient = (*metricsOVHClient)(nil)

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
