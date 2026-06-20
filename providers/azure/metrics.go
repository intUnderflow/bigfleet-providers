package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the Azure provider's Prometheus instrumentation, exposed at
// /metrics. It matches BigFleet's stack (prometheus/client_golang) so an
// operator gets Azure API visibility, RPC latency/outcomes, and the background
// loops' health out of the box.
type metrics struct {
	reg *prometheus.Registry

	azureCalls    *prometheus.CounterVec   // bigfleet_azure_api_calls_total{op,outcome}
	azureDuration *prometheus.HistogramVec // bigfleet_azure_api_duration_seconds{op}
	rpcCalls      *prometheus.CounterVec   // bigfleet_azure_grpc_requests_total{method,code}
	rpcDuration   *prometheus.HistogramVec // bigfleet_azure_grpc_request_duration_seconds{method}
	panics        prometheus.Counter       // bigfleet_azure_panics_total
	reconcile     *prometheus.CounterVec   // bigfleet_azure_reconcile_total{outcome}
	priceRefresh  *prometheus.CounterVec   // bigfleet_azure_price_refresh_total{outcome}
	interrupts    prometheus.Counter       // bigfleet_azure_spot_evictions_total
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		azureCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_azure_api_calls_total",
			Help: "Azure API calls by operation and outcome.",
		}, "op", "outcome"),
		azureDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_azure_api_duration_seconds",
			Help:    "Azure API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_azure_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_azure_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_azure_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_azure_reconcile_total",
			Help: "Background reconcile runs by outcome.",
		}, "outcome"),
		priceRefresh: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_azure_price_refresh_total",
			Help: "Background spot-price refresh runs by outcome.",
		}, "outcome"),
		interrupts: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_azure_spot_evictions_total",
			Help: "Observed Spot eviction (Scheduled Events Preempt) notices.",
		}),
	}
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *metrics) observeAzure(op string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.azureCalls.WithLabelValues(op, outcome).Inc()
	m.azureDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// metricsAzureClient decorates an azureClient, recording call counts/latency per
// operation. It is transparent — it returns exactly what the inner client does.
type metricsAzureClient struct {
	inner azureClient
	m     *metrics
}

func newMetricsAzureClient(inner azureClient, m *metrics) azureClient {
	if m == nil {
		return inner
	}
	return &metricsAzureClient{inner: inner, m: m}
}

func (c *metricsAzureClient) CreateVM(ctx context.Context, spec vmSpec) (vmInstance, error) {
	start := time.Now()
	vm, err := c.inner.CreateVM(ctx, spec)
	c.m.observeAzure("CreateVM", start, err)
	return vm, err
}
func (c *metricsAzureClient) DeleteVM(ctx context.Context, id string) error {
	start := time.Now()
	err := c.inner.DeleteVM(ctx, id)
	c.m.observeAzure("DeleteVM", start, err)
	return err
}
func (c *metricsAzureClient) DescribeManaged(ctx context.Context) ([]vmInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAzure("ListVMs", start, err)
	return out, err
}
func (c *metricsAzureClient) ApplyBootstrap(ctx context.Context, vm vmInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, vm, cluster, blob)
	c.m.observeAzure("Configure", start, err)
	return err
}
func (c *metricsAzureClient) DrainNode(ctx context.Context, vm vmInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, vm, grace)
	c.m.observeAzure("Drain", start, err)
	return err
}
func (c *metricsAzureClient) SpotPriceUSD(ctx context.Context, vmSize string) (float64, error) {
	start := time.Now()
	v, err := c.inner.SpotPriceUSD(ctx, vmSize)
	c.m.observeAzure("RetailPrices", start, err)
	return v, err
}
func (c *metricsAzureClient) DescribeVMSizeCapacities(ctx context.Context, vmSizes []string) (map[string]vmCapacity, error) {
	start := time.Now()
	out, err := c.inner.DescribeVMSizeCapacities(ctx, vmSizes)
	c.m.observeAzure("ResourceSkus", start, err)
	return out, err
}

var _ azureClient = (*metricsAzureClient)(nil)

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
