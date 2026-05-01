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
// Both are optional and independently controlled by the options passed to New.
type Provider struct {
	meterProvider otelmetric.MeterProvider
	promRegistry  *prometheus.Registry
	shutdown      func(context.Context) error
}

type config struct {
	metricsAddr  string
	otlpEndpoint string
}

// Option configures Provider construction.
type Option func(*config)

// WithMetricsAddr sets the TCP address for the Prometheus /metrics HTTP server (e.g. ":9090").
// When not supplied, no HTTP server is started.
func WithMetricsAddr(addr string) Option {
	return func(c *config) { c.metricsAddr = addr }
}

// WithOTLPEndpoint sets the OTLP gRPC endpoint URL (e.g. "http://otel-collector:4317").
// When not supplied, no OTLP exporter is created.
func WithOTLPEndpoint(endpoint string) Option {
	return func(c *config) { c.otlpEndpoint = endpoint }
}

// New creates a Provider. If neither WithMetricsAddr nor WithOTLPEndpoint is supplied,
// a no-op MeterProvider is returned.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	var (
		readers       []sdkmetric.Reader
		shutdownFuncs []func(context.Context) error
		promRegistry  *prometheus.Registry
	)

	if cfg.metricsAddr != "" {
		promRegistry = prometheus.NewRegistry()

		promReader, err := prometheusexporter.New(prometheusexporter.WithRegisterer(promRegistry))
		if err != nil {
			return nil, fmt.Errorf("create prometheus exporter: %w", err)
		}

		readers = append(readers, promReader)

		listener, err := net.Listen("tcp", cfg.metricsAddr)
		if err != nil {
			return nil, fmt.Errorf("listen on metrics addr %s: %w", cfg.metricsAddr, err)
		}

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{}))
		srv := &http.Server{Handler: mux}

		go func() {
			if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "metrics HTTP server error: %v\n", err)
			}
		}()

		shutdownFuncs = append(shutdownFuncs, srv.Shutdown)
	}

	if cfg.otlpEndpoint != "" {
		otlpExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(cfg.otlpEndpoint))
		if err != nil {
			// Shut down any already-started servers before returning. Use a fresh context
			// because ctx may already be cancelled (a common reason for init failure).
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()

			var cleanupErrs []error
			for _, shutdownFunc := range shutdownFuncs {
				cleanupErrs = append(cleanupErrs, shutdownFunc(cleanupCtx))
			}

			return nil, errors.Join(append(cleanupErrs, fmt.Errorf("create OTLP metric exporter: %w", err))...)
		}

		readers = append(readers, sdkmetric.NewPeriodicReader(otlpExporter))
	}

	if len(readers) == 0 {
		return &Provider{
			meterProvider: noop.NewMeterProvider(),
			promRegistry:  promRegistry,
			shutdown:      func(context.Context) error { return nil },
		}, nil
	}

	sdkOpts := make([]sdkmetric.Option, 0, len(readers))
	for _, reader := range readers {
		sdkOpts = append(sdkOpts, sdkmetric.WithReader(reader))
	}

	sdkProvider := sdkmetric.NewMeterProvider(sdkOpts...)
	shutdownFuncs = append(shutdownFuncs, sdkProvider.Shutdown)

	return &Provider{
		meterProvider: sdkProvider,
		promRegistry:  promRegistry,
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

// PrometheusRegisterer returns the Prometheus registry that backs the
// /metrics endpoint, or nil when no Prometheus exporter is active. Callers
// (e.g. pkg/temporalclient) use this to register additional collectors —
// such as Temporal SDK metrics via the tally→prom bridge — alongside the
// application's own OTel-sourced metrics.
//
// The return type is the prometheus.Registerer interface; a nil *Registry
// is mapped to a nil interface so callers can use a plain `if reg == nil`
// check without falling into the typed-nil interface gotcha.
func (p *Provider) PrometheusRegisterer() prometheus.Registerer {
	if p.promRegistry == nil {
		return nil
	}

	return p.promRegistry
}

// Shutdown stops all active exporters and flushes buffered metrics.
// It should be called before the process exits.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.shutdown(ctx)
}

// NewFromEnv creates a Provider using standard environment variables.
// METRICS_ADDR enables the Prometheus /metrics pull endpoint.
// OTEL_EXPORTER_OTLP_ENDPOINT enables OTLP gRPC push export.
// If neither variable is set, a no-op Provider is returned.
//
// The returned shutdown func must be deferred by the caller. It shuts down all
// exporters with a 10-second deadline and writes any error to stderr.
func NewFromEnv(ctx context.Context) (*Provider, func(), error) {
	var opts []Option
	if addr := os.Getenv("METRICS_ADDR"); addr != "" {
		opts = append(opts, WithMetricsAddr(addr))
	}

	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		opts = append(opts, WithOTLPEndpoint(endpoint))
	}

	p, err := New(ctx, opts...)
	if err != nil {
		return nil, func() {}, err
	}

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := p.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "metrics shutdown error: %v\n", err)
		}
	}

	return p, shutdown, nil
}
