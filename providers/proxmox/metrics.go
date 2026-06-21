package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the Proxmox provider's Prometheus instrumentation, exposed at
// /metrics. It matches BigFleet's stack (prometheus/client_golang) so an
// operator gets Proxmox API visibility, RPC latency/outcomes, and the background
// reconcile loop's health out of the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls    *prometheus.CounterVec   // bigfleet_proxmox_api_calls_total{op,outcome}
	apiDuration *prometheus.HistogramVec // bigfleet_proxmox_api_duration_seconds{op}
	rpcCalls    *prometheus.CounterVec   // bigfleet_proxmox_grpc_requests_total{method,code}
	rpcDuration *prometheus.HistogramVec // bigfleet_proxmox_grpc_request_duration_seconds{method}
	panics      prometheus.Counter       // bigfleet_proxmox_panics_total
	reconcile   *prometheus.CounterVec   // bigfleet_proxmox_reconcile_total{outcome}
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_proxmox_api_calls_total",
			Help: "Proxmox API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_proxmox_api_duration_seconds",
			Help:    "Proxmox API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_proxmox_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_proxmox_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_proxmox_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_proxmox_reconcile_total",
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

// metricsProxmoxClient decorates a proxmoxClient, recording call counts/latency
// per operation. It is transparent — it returns exactly what the inner client
// does.
type metricsProxmoxClient struct {
	inner proxmoxClient
	m     *metrics
}

func newMetricsProxmoxClient(inner proxmoxClient, m *metrics) proxmoxClient {
	if m == nil {
		return inner
	}
	return &metricsProxmoxClient{inner: inner, m: m}
}

func (c *metricsProxmoxClient) CloneVM(ctx context.Context, spec vmSpec) (vmInstance, error) {
	start := time.Now()
	vm, err := c.inner.CloneVM(ctx, spec)
	c.m.observeAPI("CloneVM", start, err)
	return vm, err
}
func (c *metricsProxmoxClient) DeleteVM(ctx context.Context, node string, vmid int) error {
	start := time.Now()
	err := c.inner.DeleteVM(ctx, node, vmid)
	c.m.observeAPI("DeleteVM", start, err)
	return err
}
func (c *metricsProxmoxClient) DescribeManaged(ctx context.Context) ([]vmInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("DescribeManaged", start, err)
	return out, err
}
func (c *metricsProxmoxClient) EnsureRunning(ctx context.Context, vm vmInstance) error {
	start := time.Now()
	err := c.inner.EnsureRunning(ctx, vm)
	c.m.observeAPI("EnsureRunning", start, err)
	return err
}
func (c *metricsProxmoxClient) ApplyBootstrap(ctx context.Context, vm vmInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, vm, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsProxmoxClient) DrainNode(ctx context.Context, vm vmInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, vm, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}
func (c *metricsProxmoxClient) Close() error { return c.inner.Close() }

var _ proxmoxClient = (*metricsProxmoxClient)(nil)

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
