package temporalclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.temporal.io/sdk/client"
)

// meterScope is the OTel meter scope used for all SDK-emitted instruments.
const meterScope = "go.temporal.io/sdk"

// timerUnit is the unit recorded for SDK timers. Temporal SDK consumers
// historically expect millisecond histograms (e.g. temporal_long_request_latency).
const timerUnit = "ms"

// newMetricsHandler returns a client.MetricsHandler that records SDK-emitted
// counters, gauges, and timers through the provided OTel MeterProvider. The
// instruments share whatever exporters the meter provider feeds, so passing
// in the provider from pkg/metrics surfaces SDK metrics on /metrics.
func newMetricsHandler(mp otelmetric.MeterProvider) client.MetricsHandler {
	return &metricsHandler{
		meter: mp.Meter(meterScope),
		cache: &instrumentCache{
			counters:   make(map[string]otelmetric.Int64Counter),
			gauges:     make(map[string]otelmetric.Float64Gauge),
			histograms: make(map[string]otelmetric.Float64Histogram),
		},
	}
}

// metricsHandler implements client.MetricsHandler. WithTags returns a child
// handler that shares the same instrument cache but layers additional
// attributes onto each recording.
type metricsHandler struct {
	meter otelmetric.Meter
	cache *instrumentCache
	attrs attribute.Set
}

// instrumentCache memoises OTel instruments by name. Sharing the cache
// across child handlers ensures every WithTags variant records into the
// same underlying instrument and the SDK doesn't pay for instrument
// re-resolution on every Counter/Gauge/Timer call.
type instrumentCache struct {
	mu         sync.Mutex
	counters   map[string]otelmetric.Int64Counter
	gauges     map[string]otelmetric.Float64Gauge
	histograms map[string]otelmetric.Float64Histogram
}

func (h *metricsHandler) WithTags(tags map[string]string) client.MetricsHandler {
	if len(tags) == 0 {
		return h
	}

	merged := make(map[attribute.Key]attribute.Value, h.attrs.Len()+len(tags))

	for _, kv := range h.attrs.ToSlice() {
		merged[kv.Key] = kv.Value
	}

	for k, v := range tags {
		merged[attribute.Key(k)] = attribute.StringValue(v)
	}

	kvs := make([]attribute.KeyValue, 0, len(merged))
	for k, v := range merged {
		kvs = append(kvs, attribute.KeyValue{Key: k, Value: v})
	}

	return &metricsHandler{
		meter: h.meter,
		cache: h.cache,
		attrs: attribute.NewSet(kvs...),
	}
}

func (h *metricsHandler) Counter(name string) client.MetricsCounter {
	return counterAdapter{counter: h.counter(name), attrs: h.measurementOpt()}
}

func (h *metricsHandler) Gauge(name string) client.MetricsGauge {
	return gaugeAdapter{gauge: h.gauge(name), attrs: h.measurementOpt()}
}

func (h *metricsHandler) Timer(name string) client.MetricsTimer {
	return timerAdapter{histogram: h.histogram(name), attrs: h.measurementOpt()}
}

func (h *metricsHandler) measurementOpt() otelmetric.MeasurementOption {
	return otelmetric.WithAttributeSet(h.attrs)
}

func (h *metricsHandler) counter(name string) otelmetric.Int64Counter {
	h.cache.mu.Lock()
	defer h.cache.mu.Unlock()

	if c, ok := h.cache.counters[name]; ok {
		return c
	}

	c, err := h.meter.Int64Counter(name)
	if err != nil {
		// OTel only returns an error here when the instrument name conflicts
		// with an incompatible existing instrument — a programmer/config bug,
		// not a runtime condition the SDK can recover from.
		panic(fmt.Errorf("create temporal counter %q: %w", name, err))
	}

	h.cache.counters[name] = c

	return c
}

func (h *metricsHandler) gauge(name string) otelmetric.Float64Gauge {
	h.cache.mu.Lock()
	defer h.cache.mu.Unlock()

	if g, ok := h.cache.gauges[name]; ok {
		return g
	}

	g, err := h.meter.Float64Gauge(name)
	if err != nil {
		panic(fmt.Errorf("create temporal gauge %q: %w", name, err))
	}

	h.cache.gauges[name] = g

	return g
}

func (h *metricsHandler) histogram(name string) otelmetric.Float64Histogram {
	h.cache.mu.Lock()
	defer h.cache.mu.Unlock()

	if hist, ok := h.cache.histograms[name]; ok {
		return hist
	}

	hist, err := h.meter.Float64Histogram(name, otelmetric.WithUnit(timerUnit))
	if err != nil {
		panic(fmt.Errorf("create temporal histogram %q: %w", name, err))
	}

	h.cache.histograms[name] = hist

	return hist
}

type counterAdapter struct {
	counter otelmetric.Int64Counter
	attrs   otelmetric.MeasurementOption
}

func (c counterAdapter) Inc(d int64) {
	c.counter.Add(context.Background(), d, c.attrs)
}

type gaugeAdapter struct {
	gauge otelmetric.Float64Gauge
	attrs otelmetric.MeasurementOption
}

func (g gaugeAdapter) Update(v float64) {
	g.gauge.Record(context.Background(), v, g.attrs)
}

type timerAdapter struct {
	histogram otelmetric.Float64Histogram
	attrs     otelmetric.MeasurementOption
}

func (t timerAdapter) Record(d time.Duration) {
	t.histogram.Record(context.Background(), float64(d)/float64(time.Millisecond), t.attrs)
}
