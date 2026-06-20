package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the Hetzner provider's Prometheus instrumentation, exposed at
// /metrics. It matches BigFleet's stack (prometheus/client_golang) so an
// operator gets Hetzner Cloud API visibility, RPC latency/outcomes, and the
// background loops' health out of the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls     *prometheus.CounterVec   // bigfleet_hetzner_api_calls_total{op,outcome}
	apiDuration  *prometheus.HistogramVec // bigfleet_hetzner_api_duration_seconds{op}
	rpcCalls     *prometheus.CounterVec   // bigfleet_hetzner_grpc_requests_total{method,code}
	rpcDuration  *prometheus.HistogramVec // bigfleet_hetzner_grpc_request_duration_seconds{method}
	panics       prometheus.Counter       // bigfleet_hetzner_panics_total
	reconcile    *prometheus.CounterVec   // bigfleet_hetzner_reconcile_total{outcome}
	priceRefresh *prometheus.CounterVec   // bigfleet_hetzner_price_refresh_total{outcome}
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_hetzner_api_calls_total",
			Help: "Hetzner Cloud API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_hetzner_api_duration_seconds",
			Help:    "Hetzner Cloud API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_hetzner_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_hetzner_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_hetzner_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_hetzner_reconcile_total",
			Help: "Background reconcile runs by outcome.",
		}, "outcome"),
		priceRefresh: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_hetzner_price_refresh_total",
			Help: "Background price-refresh runs by outcome.",
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

// metricsHCloudClient decorates an hcloudClient, recording call counts/latency
// per operation. It is transparent — it returns exactly what the inner client
// does.
type metricsHCloudClient struct {
	inner hcloudClient
	m     *metrics
}

func newMetricsHCloudClient(inner hcloudClient, m *metrics) hcloudClient {
	if m == nil {
		return inner
	}
	return &metricsHCloudClient{inner: inner, m: m}
}

func (c *metricsHCloudClient) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	start := time.Now()
	srv, err := c.inner.CreateServer(ctx, spec)
	c.m.observeAPI("CreateServer", start, err)
	return srv, err
}
func (c *metricsHCloudClient) DeleteServer(ctx context.Context, id string) error {
	start := time.Now()
	err := c.inner.DeleteServer(ctx, id)
	c.m.observeAPI("DeleteServer", start, err)
	return err
}
func (c *metricsHCloudClient) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("DescribeManaged", start, err)
	return out, err
}
func (c *metricsHCloudClient) ApplyBootstrap(ctx context.Context, srv serverInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, srv, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsHCloudClient) DrainNode(ctx context.Context, srv serverInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, srv, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}
func (c *metricsHCloudClient) PriceUSD(ctx context.Context, serverType, location string) (float64, error) {
	start := time.Now()
	v, err := c.inner.PriceUSD(ctx, serverType, location)
	c.m.observeAPI("ServerTypePricing", start, err)
	return v, err
}
func (c *metricsHCloudClient) DescribeServerTypeCapacities(ctx context.Context, serverTypes []string) (map[string]serverCapacity, error) {
	start := time.Now()
	out, err := c.inner.DescribeServerTypeCapacities(ctx, serverTypes)
	c.m.observeAPI("ServerType", start, err)
	return out, err
}

var _ hcloudClient = (*metricsHCloudClient)(nil)

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
