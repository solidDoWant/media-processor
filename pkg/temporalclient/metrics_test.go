package temporalclient

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	prometheusexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// newTestMeterProvider builds an OTel MeterProvider whose readings are
// gathered via a Prometheus registry, so tests can introspect what the
// adapter actually emitted by inspecting the Prometheus output.
func newTestMeterProvider(t *testing.T) (*sdkmetric.MeterProvider, *prometheus.Registry) {
	t.Helper()

	registry := prometheus.NewRegistry()

	reader, err := prometheusexporter.New(prometheusexporter.WithRegisterer(registry))
	require.NoError(t, err)

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() {
		_ = provider.Shutdown(t.Context())
	})

	return provider, registry
}

func TestMetricsHandlerCounter(t *testing.T) {
	provider, registry := newTestMeterProvider(t)

	handler := newMetricsHandler(provider)
	counter := handler.Counter("temporal_request")
	counter.Inc(3)
	counter.Inc(2)

	gathered := gather(t, registry)

	mf := findMetricFamily(t, gathered, "temporal_request_total")
	require.Len(t, mf.GetMetric(), 1)
	assert.Equal(t, float64(5), mf.GetMetric()[0].GetCounter().GetValue())
}

func TestMetricsHandlerGauge(t *testing.T) {
	provider, registry := newTestMeterProvider(t)

	handler := newMetricsHandler(provider)
	gauge := handler.Gauge("temporal_workers_busy")
	gauge.Update(7)
	gauge.Update(4) // most recent value wins for a synchronous gauge

	gathered := gather(t, registry)

	mf := findMetricFamily(t, gathered, "temporal_workers_busy")
	require.Len(t, mf.GetMetric(), 1)
	assert.Equal(t, float64(4), mf.GetMetric()[0].GetGauge().GetValue())
}

func TestMetricsHandlerTimerRecordsMilliseconds(t *testing.T) {
	provider, registry := newTestMeterProvider(t)

	handler := newMetricsHandler(provider)
	timer := handler.Timer("temporal_long_request_latency")
	timer.Record(250 * time.Millisecond)

	gathered := gather(t, registry)

	mf := findMetricFamily(t, gathered, "temporal_long_request_latency_milliseconds")
	require.Len(t, mf.GetMetric(), 1)

	hist := mf.GetMetric()[0].GetHistogram()
	require.NotNil(t, hist)
	assert.Equal(t, uint64(1), hist.GetSampleCount())
	assert.InDelta(t, 250.0, hist.GetSampleSum(), 0.001)
}

func TestMetricsHandlerWithTagsLayersAttributes(t *testing.T) {
	provider, registry := newTestMeterProvider(t)

	root := newMetricsHandler(provider)
	tagged := root.WithTags(map[string]string{"namespace": "default", "task_queue": "media"}).
		WithTags(map[string]string{"task_queue": "override"}) // child overrides parent

	tagged.Counter("temporal_request").Inc(1)

	gathered := gather(t, registry)

	mf := findMetricFamily(t, gathered, "temporal_request_total")
	require.Len(t, mf.GetMetric(), 1)

	labels := map[string]string{}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}

	assert.Equal(t, "default", labels["namespace"])
	assert.Equal(t, "override", labels["task_queue"], "child WithTags should overwrite parent values for the same key")
}

func TestMetricsHandlerWithEmptyTagsReturnsSameHandler(t *testing.T) {
	provider, _ := newTestMeterProvider(t)

	root := newMetricsHandler(provider)
	same := root.WithTags(nil)

	assert.Same(t, root, same)
}

func TestMetricsHandlerInstrumentsAreSharedAcrossChildren(t *testing.T) {
	provider, registry := newTestMeterProvider(t)

	root := newMetricsHandler(provider)
	root.WithTags(map[string]string{"namespace": "a"}).Counter("temporal_request").Inc(1)
	root.WithTags(map[string]string{"namespace": "b"}).Counter("temporal_request").Inc(2)

	gathered := gather(t, registry)

	mf := findMetricFamily(t, gathered, "temporal_request_total")
	// Two distinct attribute sets ⇒ two timeseries on a single metric family,
	// confirming the underlying instrument is shared and only the attributes
	// vary per child.
	assert.Len(t, mf.GetMetric(), 2)
}

// gather collects the registry's metric families, failing the test if the
// gather itself errors.
func gather(t *testing.T, registry *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()

	mfs, err := registry.Gather()
	require.NoError(t, err)

	return mfs
}

// findMetricFamily looks up a metric family by its Prometheus name; if the
// family is missing the test fails with a list of what was actually gathered,
// so the failure message points to the likely cause (e.g. wrong unit suffix).
func findMetricFamily(t *testing.T, gathered []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()

	for _, mf := range gathered {
		if mf.GetName() == name {
			return mf
		}
	}

	names := make([]string, 0, len(gathered))
	for _, mf := range gathered {
		names = append(names, mf.GetName())
	}

	t.Fatalf("metric family %q not present in gathered output:\n%s", name, strings.Join(names, "\n"))

	return nil
}
