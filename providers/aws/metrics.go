package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics is the AWS provider's Prometheus instrumentation, exposed at
// /metrics. It matches BigFleet's stack (prometheus/client_golang) so an
// operator gets EC2 API visibility, RPC latency/outcomes, and the background
// loops' health out of the box.
type metrics struct {
	reg *prometheus.Registry

	ec2Calls    *prometheus.CounterVec   // bigfleet_aws_ec2_api_calls_total{op,outcome}
	ec2Duration *prometheus.HistogramVec // bigfleet_aws_ec2_api_duration_seconds{op}
	rpcCalls    *prometheus.CounterVec   // bigfleet_aws_grpc_requests_total{method,code}
	rpcDuration *prometheus.HistogramVec // bigfleet_aws_grpc_request_duration_seconds{method}
	panics      prometheus.Counter       // bigfleet_aws_panics_total
	reconcile   *prometheus.CounterVec   // bigfleet_aws_reconcile_total{outcome}
	spotRefresh *prometheus.CounterVec   // bigfleet_aws_spot_refresh_total{outcome}
	interrupts  prometheus.Counter       // bigfleet_aws_spot_interruptions_total
}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	f := promauto(reg)
	m := &metrics{
		reg: reg,
		ec2Calls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_aws_ec2_api_calls_total",
			Help: "EC2/SSM API calls by operation and outcome.",
		}, "op", "outcome"),
		ec2Duration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_aws_ec2_api_duration_seconds",
			Help:    "EC2/SSM API call latency by operation.",
			Buckets: prometheus.DefBuckets,
		}, "op"),
		rpcCalls: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_aws_grpc_requests_total",
			Help: "CapacityProvider gRPC requests by method and status code.",
		}, "method", "code"),
		rpcDuration: f.histogramVec(prometheus.HistogramOpts{
			Name:    "bigfleet_aws_grpc_request_duration_seconds",
			Help:    "CapacityProvider gRPC request latency by method.",
			Buckets: prometheus.DefBuckets,
		}, "method"),
		panics: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_aws_panics_total",
			Help: "Recovered panics in gRPC handlers.",
		}),
		reconcile: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_aws_reconcile_total",
			Help: "Background reconcile runs by outcome.",
		}, "outcome"),
		spotRefresh: f.counterVec(prometheus.CounterOpts{
			Name: "bigfleet_aws_spot_refresh_total",
			Help: "Background spot-price refresh runs by outcome.",
		}, "outcome"),
		interrupts: f.counter(prometheus.CounterOpts{
			Name: "bigfleet_aws_spot_interruptions_total",
			Help: "Observed spot interruption / rebalance notices.",
		}),
	}
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *metrics) observeEC2(op string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.ec2Calls.WithLabelValues(op, outcome).Inc()
	m.ec2Duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// metricsEC2Client decorates an ec2Client, recording call counts/latency per
// operation. It is transparent — it returns exactly what the inner client does.
type metricsEC2Client struct {
	inner ec2Client
	m     *metrics
}

func newMetricsEC2Client(inner ec2Client, m *metrics) ec2Client {
	if m == nil {
		return inner
	}
	return &metricsEC2Client{inner: inner, m: m}
}

func (c *metricsEC2Client) RunInstance(ctx context.Context, spec runSpec) (ec2Instance, error) {
	start := time.Now()
	inst, err := c.inner.RunInstance(ctx, spec)
	c.m.observeEC2("RunInstances", start, err)
	return inst, err
}
func (c *metricsEC2Client) TerminateInstance(ctx context.Context, id string) error {
	start := time.Now()
	err := c.inner.TerminateInstance(ctx, id)
	c.m.observeEC2("TerminateInstances", start, err)
	return err
}
func (c *metricsEC2Client) DescribeManaged(ctx context.Context) ([]ec2Instance, error) {
	start := time.Now()
	out, err := c.inner.DescribeManaged(ctx)
	c.m.observeEC2("DescribeInstances", start, err)
	return out, err
}
func (c *metricsEC2Client) ApplyBootstrap(ctx context.Context, id, cluster string, blob []byte) error {
	start := time.Now()
	err := c.inner.ApplyBootstrap(ctx, id, cluster, blob)
	c.m.observeEC2("Configure", start, err)
	return err
}
func (c *metricsEC2Client) DrainNode(ctx context.Context, id string, grace int64) error {
	start := time.Now()
	err := c.inner.DrainNode(ctx, id, grace)
	c.m.observeEC2("Drain", start, err)
	return err
}
func (c *metricsEC2Client) SpotPriceUSD(ctx context.Context, instanceType, zone string) (float64, error) {
	start := time.Now()
	v, err := c.inner.SpotPriceUSD(ctx, instanceType, zone)
	c.m.observeEC2("DescribeSpotPriceHistory", start, err)
	return v, err
}
func (c *metricsEC2Client) DescribeInstanceCapacities(ctx context.Context, instanceTypes []string) (map[string]instanceCapacity, error) {
	start := time.Now()
	out, err := c.inner.DescribeInstanceCapacities(ctx, instanceTypes)
	c.m.observeEC2("DescribeInstanceTypes", start, err)
	return out, err
}

var _ ec2Client = (*metricsEC2Client)(nil)

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
