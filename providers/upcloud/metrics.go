package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the UpCloud provider's Prometheus instrumentation, exposed at
// /metrics on an isolated registry. It matches BigFleet's stack
// (prometheus/client_golang) so an operator gets UpCloud API visibility, RPC
// latency/outcomes, and the background loops' health out of the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls    *prometheus.CounterVec   // bigfleet_upcloud_api_calls_total{op,outcome}
	apiDuration *prometheus.HistogramVec // bigfleet_upcloud_api_duration_seconds{op}
	rpcCalls    *prometheus.CounterVec   // bigfleet_upcloud_grpc_requests_total{method,code}
	rpcDuration *prometheus.HistogramVec // bigfleet_upcloud_grpc_request_duration_seconds{method}
	panics      prometheus.Counter       // bigfleet_upcloud_panics_total
	reconcile   *prometheus.CounterVec   // bigfleet_upcloud_reconcile_total{outcome}

	priceRefresh     *prometheus.CounterVec // bigfleet_upcloud_price_refresh_total{outcome}
	priceLastSuccess prometheus.Gauge       // bigfleet_upcloud_price_refresh_last_success_timestamp_seconds
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_upcloud_api_calls_total",
			Help: "UpCloud API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_upcloud_api_duration_seconds",
			Help:    "UpCloud API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_upcloud_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_upcloud_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_upcloud_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_upcloud_reconcile_total",
			Help: "Background reconcile runs by outcome.",
		}, "outcome"),
		priceRefresh: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_upcloud_price_refresh_total",
			Help: "Background live price-refresh runs by outcome.",
		}, "outcome"),
		priceLastSuccess: f.gauge(prometheus.GaugeOpts{
			Name: "bigfleet_upcloud_price_refresh_last_success_timestamp_seconds",
			Help: "Unix time of the last successful live price refresh (staleness age = now - this).",
		}),
	}
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// recordPriceRefresh records a price-refresh outcome: a non-zero failure count is
// an error (an API failure or a genuinely unpriced plan), otherwise success — and
// a success stamps the last-success gauge so staleness (age = now - gauge) is
// observable.
func (m *metrics) recordPriceRefresh(failed int) {
	if failed > 0 {
		m.priceRefresh.WithLabelValues("error").Inc()
		return
	}
	m.priceRefresh.WithLabelValues("success").Inc()
	m.priceLastSuccess.SetToCurrentTime()
}

func (m *metrics) observeAPI(op string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.apiCalls.WithLabelValues(op, outcome).Inc()
	m.apiDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// metricsUpcloudClient decorates an upcloudClient, recording call counts/latency
// per operation. It is transparent — it returns exactly what the inner client
// does.
type metricsUpcloudClient struct {
	inner upcloudClient
	m     *metrics
}

func newMetricsUpcloudClient(inner upcloudClient, m *metrics) upcloudClient {
	if m == nil {
		return inner
	}
	return &metricsUpcloudClient{inner: inner, m: m}
}

func (c *metricsUpcloudClient) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	start := time.Now()
	srv, err := c.inner.CreateServer(ctx, spec)
	c.m.observeAPI("CreateServer", start, err)
	return srv, err
}
func (c *metricsUpcloudClient) DeleteServer(ctx context.Context, uuid string) error {
	start := time.Now()
	err := c.inner.DeleteServer(ctx, uuid)
	c.m.observeAPI("DeleteServer", start, err)
	return err
}
func (c *metricsUpcloudClient) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("DescribeManaged", start, err)
	return out, err
}
func (c *metricsUpcloudClient) EnsureRunning(ctx context.Context, srv serverInstance) (serverInstance, error) {
	start := time.Now()
	out, err := c.inner.EnsureRunning(ctx, srv)
	c.m.observeAPI("EnsureRunning", start, err)
	return out, err
}
func (c *metricsUpcloudClient) ApplyBootstrap(ctx context.Context, srv serverInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, srv, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsUpcloudClient) DrainNode(ctx context.Context, srv serverInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, srv, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}
func (c *metricsUpcloudClient) DescribePlanCapacities(ctx context.Context, plans []string) (map[string]planCapacity, error) {
	start := time.Now()
	out, err := c.inner.DescribePlanCapacities(ctx, plans)
	c.m.observeAPI("Plans", start, err)
	return out, err
}
func (c *metricsUpcloudClient) DescribePlanPrices(ctx context.Context, plans []string) (map[string]float64, error) {
	start := time.Now()
	out, err := c.inner.DescribePlanPrices(ctx, plans)
	c.m.observeAPI("Prices", start, err)
	return out, err
}

var _ upcloudClient = (*metricsUpcloudClient)(nil)

// promFactory registers each metric on a specific registry (so the provider uses
// an isolated registry, not the global default).
type promFactory struct{ reg *prometheus.Registry }

func promauto(reg *prometheus.Registry) promFactory { return promFactory{reg} }

func (f promFactory) counter(o prometheus.CounterOpts) prometheus.Counter {
	c := prometheus.NewCounter(o)
	f.reg.MustRegister(c)
	return c
}
func (f promFactory) gauge(o prometheus.GaugeOpts) prometheus.Gauge {
	g := prometheus.NewGauge(o)
	f.reg.MustRegister(g)
	return g
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
