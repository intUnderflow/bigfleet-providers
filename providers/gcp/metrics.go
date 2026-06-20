package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the GCP provider's Prometheus instrumentation, exposed at /metrics.
// It matches BigFleet's stack (prometheus/client_golang) so an operator gets GCE
// API visibility, RPC latency/outcomes, and the background loops' health out of
// the box.
type metrics struct {
	reg *prometheus.Registry

	apiCalls    *prometheus.CounterVec   // bigfleet_gcp_api_calls_total{op,outcome}
	apiDuration *prometheus.HistogramVec // bigfleet_gcp_api_duration_seconds{op}
	rpcCalls    *prometheus.CounterVec   // bigfleet_gcp_grpc_requests_total{method,code}
	rpcDuration *prometheus.HistogramVec // bigfleet_gcp_grpc_request_duration_seconds{method}
	panics      prometheus.Counter       // bigfleet_gcp_panics_total
	reconcile   *prometheus.CounterVec   // bigfleet_gcp_reconcile_total{outcome}
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		apiCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_gcp_api_calls_total",
			Help: "GCE API calls by operation and outcome.",
		}, "op", "outcome"),
		apiDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_gcp_api_duration_seconds",
			Help:    "GCE API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_gcp_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_gcp_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_gcp_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_gcp_reconcile_total",
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

// metricsGCEClient decorates a gceClient, recording call counts/latency per
// operation. It is transparent — it returns exactly what the inner client does.
type metricsGCEClient struct {
	inner gceClient
	m     *metrics
}

func newMetricsGCEClient(inner gceClient, m *metrics) gceClient {
	if m == nil {
		return inner
	}
	return &metricsGCEClient{inner: inner, m: m}
}

func (c *metricsGCEClient) Insert(ctx context.Context, spec instanceSpec) (gceInstance, error) {
	start := time.Now()
	inst, err := c.inner.Insert(ctx, spec)
	c.m.observeAPI("Insert", start, err)
	return inst, err
}
func (c *metricsGCEClient) DeleteInstance(ctx context.Context, zone, name string) error {
	start := time.Now()
	err := c.inner.DeleteInstance(ctx, zone, name)
	c.m.observeAPI("Delete", start, err)
	return err
}
func (c *metricsGCEClient) DescribeManaged(ctx context.Context) ([]gceInstance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeAPI("List", start, err)
	return out, err
}
func (c *metricsGCEClient) ApplyBootstrap(ctx context.Context, inst gceInstance, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, inst, cluster, blob)
	c.m.observeAPI("Configure", start, err)
	return err
}
func (c *metricsGCEClient) DrainNode(ctx context.Context, inst gceInstance, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, inst, grace)
	c.m.observeAPI("Drain", start, err)
	return err
}
func (c *metricsGCEClient) DescribeMachineTypeCapacities(ctx context.Context, refs []machineTypeRef) (map[string]machineCapacity, error) {
	start := time.Now()
	out, err := c.inner.DescribeMachineTypeCapacities(ctx, refs)
	c.m.observeAPI("MachineTypes", start, err)
	return out, err
}

var _ gceClient = (*metricsGCEClient)(nil)

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
