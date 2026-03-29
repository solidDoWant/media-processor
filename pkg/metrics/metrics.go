package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

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

type config struct {
	prometheusListener net.Listener
}

// Option configures Provider construction.
type Option func(*config)

// WithPrometheusListener supplies a pre-bound listener for the Prometheus HTTP server.
// When set, METRICS_ADDR is still required to enable the server, but the given listener
// is used instead of binding a new one. Primarily useful in tests to eliminate TOCTOU races.
func WithPrometheusListener(l net.Listener) Option {
	return func(c *config) {
		c.prometheusListener = l
	}
}

// New creates a Provider based on environment variables. If neither METRICS_ADDR nor
// OTEL_EXPORTER_OTLP_ENDPOINT is set, a no-op MeterProvider is returned.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}

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

		l := cfg.prometheusListener
		if l == nil {
			l, err = net.Listen("tcp", metricsAddr)
			if err != nil {
				return nil, fmt.Errorf("listen on metrics addr %s: %w", metricsAddr, err)
			}
		}

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(l) }()
		shutdownFuncs = append(shutdownFuncs, srv.Shutdown)
	}

	otlpEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otlpEndpoint != "" {
		otlpExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(otlpEndpoint))
		if err != nil {
			// Shut down any already-started servers before returning. Use a fresh context
			// because ctx may already be cancelled (a common reason for init failure).
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			for _, fn := range shutdownFuncs {
				_ = fn(cleanupCtx)
			}
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

	opts2 := make([]sdkmetric.Option, 0, len(readers))
	for _, r := range readers {
		opts2 = append(opts2, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts2...)
	shutdownFuncs = append(shutdownFuncs, mp.Shutdown)

	return &Provider{
		meterProvider: mp,
		shutdown: func(ctx context.Context) error {
			var errs []error
			// Shut down in reverse (LIFO) order so the OTel MeterProvider is flushed
			// before the Prometheus HTTP server is stopped, giving the full context
			// deadline to the OTLP flush rather than splitting it with HTTP shutdown.
			for i := len(shutdownFuncs) - 1; i >= 0; i-- {
				errs = append(errs, shutdownFuncs[i](ctx))
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
