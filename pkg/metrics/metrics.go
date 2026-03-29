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
	shutdown      func(context.Context) error
}

type config struct {
	metricsAddr        string
	otlpEndpoint       string
	prometheusListener net.Listener
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

// WithPrometheusListener supplies a pre-bound listener for the Prometheus HTTP server.
// WithMetricsAddr must also be supplied to enable the server; the provided listener is
// used instead of binding a new one. The caller is responsible for ensuring the listener
// address matches the addr passed to WithMetricsAddr. Primarily useful in tests to
// eliminate TOCTOU races.
func WithPrometheusListener(l net.Listener) Option {
	return func(c *config) { c.prometheusListener = l }
}

// New creates a Provider. If neither WithMetricsAddr nor WithOTLPEndpoint is supplied,
// a no-op MeterProvider is returned.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}

	var readers []sdkmetric.Reader
	var shutdownFuncs []func(context.Context) error

	if cfg.metricsAddr != "" {
		promRegistry := prometheus.NewRegistry()
		promReader, err := prometheusexporter.New(prometheusexporter.WithRegisterer(promRegistry))
		if err != nil {
			return nil, fmt.Errorf("create prometheus exporter: %w", err)
		}
		readers = append(readers, promReader)

		listener := cfg.prometheusListener
		if listener == nil {
			listener, err = net.Listen("tcp", cfg.metricsAddr)
			if err != nil {
				return nil, fmt.Errorf("listen on metrics addr %s: %w", cfg.metricsAddr, err)
			}
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
