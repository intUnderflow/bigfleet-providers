package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the libvirt provider's Prometheus instrumentation, exposed at
// /metrics. It matches BigFleet's stack (prometheus/client_golang) so an
// operator gets libvirt API visibility, RPC latency/outcomes, and the background
// reconcile loop's health out of the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls    *prometheus.CounterVec   // bigfleet_libvirt_api_calls_total{op,outcome}
	apiDuration *prometheus.HistogramVec // bigfleet_libvirt_api_duration_seconds{op}
	rpcCalls    *prometheus.CounterVec   // bigfleet_libvirt_grpc_requests_total{method,code}
	rpcDuration *prometheus.HistogramVec // bigfleet_libvirt_grpc_request_duration_seconds{method}
	panics      prometheus.Counter       // bigfleet_libvirt_panics_total
	reconcile   *prometheus.CounterVec   // bigfleet_libvirt_reconcile_total{outcome}
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_libvirt_api_calls_total",
			Help: "libvirt API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_libvirt_api_duration_seconds",
			Help:    "libvirt API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_libvirt_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_libvirt_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_libvirt_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_libvirt_reconcile_total",
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

// metricsLibvirtClient decorates a libvirtClient, recording call counts/latency
// per operation. It is transparent — it returns exactly what the inner client
// does.
type metricsLibvirtClient struct {
	inner libvirtClient
	m     *metrics
}

func newMetricsLibvirtClient(inner libvirtClient, m *metrics) libvirtClient {
	if m == nil {
		return inner
	}
	return &metricsLibvirtClient{inner: inner, m: m}
}

func (c *metricsLibvirtClient) CreateDomain(ctx context.Context, spec domainSpec) (domainInstance, error) {
	start := time.Now()
	dom, err := c.inner.CreateDomain(ctx, spec)
	c.m.observeAPI("CreateDomain", start, err)
	return dom, err
}
func (c *metricsLibvirtClient) DeleteDomain(ctx context.Context, zone, domainName string) error {
	start := time.Now()
	err := c.inner.DeleteDomain(ctx, zone, domainName)
	c.m.observeAPI("DeleteDomain", start, err)
	return err
}
func (c *metricsLibvirtClient) DescribeManaged(ctx context.Context) ([]domainInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("DescribeManaged", start, err)
	return out, err
}
func (c *metricsLibvirtClient) EnsureRunning(ctx context.Context, dom domainInstance) error {
	start := time.Now()
	err := c.inner.EnsureRunning(ctx, dom)
	c.m.observeAPI("EnsureRunning", start, err)
	return err
}
func (c *metricsLibvirtClient) ApplyBootstrap(ctx context.Context, dom domainInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, dom, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsLibvirtClient) DrainNode(ctx context.Context, dom domainInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, dom, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}
func (c *metricsLibvirtClient) Close() error { return c.inner.Close() }

var _ libvirtClient = (*metricsLibvirtClient)(nil)

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
