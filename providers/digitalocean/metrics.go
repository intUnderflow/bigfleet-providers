package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the DigitalOcean provider's Prometheus instrumentation, exposed at
// /metrics on an isolated registry. It matches BigFleet's stack
// (prometheus/client_golang) so an operator gets DigitalOcean API visibility, RPC
// latency/outcomes, and the background loops' health out of the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls     *prometheus.CounterVec   // bigfleet_digitalocean_api_calls_total{op,outcome}
	apiDuration  *prometheus.HistogramVec // bigfleet_digitalocean_api_duration_seconds{op}
	rpcCalls     *prometheus.CounterVec   // bigfleet_digitalocean_grpc_requests_total{method,code}
	rpcDuration  *prometheus.HistogramVec // bigfleet_digitalocean_grpc_request_duration_seconds{method}
	panics       prometheus.Counter       // bigfleet_digitalocean_panics_total
	reconcile    *prometheus.CounterVec   // bigfleet_digitalocean_reconcile_total{outcome}
	priceRefresh *prometheus.CounterVec   // bigfleet_digitalocean_price_refresh_total{outcome}
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_digitalocean_api_calls_total",
			Help: "DigitalOcean API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_digitalocean_api_duration_seconds",
			Help:    "DigitalOcean API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_digitalocean_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_digitalocean_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_digitalocean_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_digitalocean_reconcile_total",
			Help: "Background reconcile runs by outcome.",
		}, "outcome"),
		priceRefresh: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_digitalocean_price_refresh_total",
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

// metricsDOClient decorates a doClient, recording call counts/latency per
// operation. It is transparent — it returns exactly what the inner client does.
type metricsDOClient struct {
	inner doClient
	m     *metrics
}

func newMetricsDOClient(inner doClient, m *metrics) doClient {
	if m == nil {
		return inner
	}
	return &metricsDOClient{inner: inner, m: m}
}

func (c *metricsDOClient) CreateDroplet(ctx context.Context, spec dropletSpec) (dropletInstance, error) {
	start := time.Now()
	drv, err := c.inner.CreateDroplet(ctx, spec)
	c.m.observeAPI("CreateDroplet", start, err)
	return drv, err
}
func (c *metricsDOClient) DeleteDroplet(ctx context.Context, id string) error {
	start := time.Now()
	err := c.inner.DeleteDroplet(ctx, id)
	c.m.observeAPI("DeleteDroplet", start, err)
	return err
}
func (c *metricsDOClient) DescribeManaged(ctx context.Context) ([]dropletInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("DescribeManaged", start, err)
	return out, err
}
func (c *metricsDOClient) ApplyBootstrap(ctx context.Context, drv dropletInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, drv, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsDOClient) DrainNode(ctx context.Context, drv dropletInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, drv, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}
func (c *metricsDOClient) PriceUSD(ctx context.Context, sizeSlug string) (float64, error) {
	start := time.Now()
	v, err := c.inner.PriceUSD(ctx, sizeSlug)
	c.m.observeAPI("SizePricing", start, err)
	return v, err
}
func (c *metricsDOClient) DescribeSizeCapacities(ctx context.Context, sizeSlugs []string) (map[string]sizeCapacity, error) {
	start := time.Now()
	out, err := c.inner.DescribeSizeCapacities(ctx, sizeSlugs)
	c.m.observeAPI("Sizes", start, err)
	return out, err
}

var _ doClient = (*metricsDOClient)(nil)

// promFactory registers each metric on a specific registry (so the provider uses
// an isolated registry, not the global default).
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
