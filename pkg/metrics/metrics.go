package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	prometheusexporter "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Provider manages metrics exporters: a Prometheus pull endpoint and/or an OTLP push exporter.
// Both are optional and independently controlled by environment variables:
//   - METRICS_ADDR: if set, starts a Prometheus /metrics HTTP server at that address.
//   - OTEL_EXPORTER_OTLP_ENDPOINT: if set, initialises an OTLP metric exporter.
type Provider struct {
	meterProvider otelmetric.MeterProvider
	shutdown      func(context.Context) error
}

// New creates a Provider based on environment variables. If neither METRICS_ADDR nor
// OTEL_EXPORTER_OTLP_ENDPOINT is set, a no-op MeterProvider is returned.
func New(ctx context.Context) (*Provider, error) {
	var readers []sdkmetric.Reader
	var shutdownFuncs []func(context.Context) error

	metricsAddr := os.Getenv("METRICS_ADDR")
	if metricsAddr != "" {
		reg := prometheus.NewRegistry()
		promReader, err := prometheusexporter.New(prometheusexporter.WithRegisterer(reg))
		if err != nil {
			return nil, fmt.Errorf("create prometheus exporter: %w", err)
		}
		readers = append(readers, promReader)

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		srv := &http.Server{
			Addr:    metricsAddr,
			Handler: mux,
		}
		go func() { _ = srv.ListenAndServe() }()
		shutdownFuncs = append(shutdownFuncs, srv.Shutdown)
	}

	otlpEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otlpEndpoint != "" {
		otlpExporter, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
		}
		readers = append(readers, sdkmetric.NewPeriodicReader(otlpExporter))
	}

	if len(readers) == 0 {
		return &Provider{
			meterProvider: noop.NewMeterProvider(),
			shutdown:      func(context.Context) error { return nil },
		}, nil
	}

	opts := make([]sdkmetric.Option, 0, len(readers))
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	shutdownFuncs = append(shutdownFuncs, mp.Shutdown)

	return &Provider{
		meterProvider: mp,
		shutdown: func(ctx context.Context) error {
			var errs []error
			for _, fn := range shutdownFuncs {
				errs = append(errs, fn(ctx))
			}
			return errors.Join(errs...)
		},
	}, nil
}

// MeterProvider returns the underlying OTel MeterProvider.
func (p *Provider) MeterProvider() otelmetric.MeterProvider {
	return p.meterProvider
}

// Shutdown stops all active exporters and flushes buffered metrics.
// It should be called before the process exits.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.shutdown(ctx)
}
