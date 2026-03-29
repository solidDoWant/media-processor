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

// Config holds the configuration for the metrics Provider.
type Config struct {
	// MetricsAddr is the TCP address for the Prometheus /metrics HTTP server (e.g. ":9090").
	// When empty, no HTTP server is started.
	MetricsAddr string

	// OTLPEndpoint is the OTLP gRPC endpoint URL (e.g. "http://otel-collector:4317").
	// When empty, no OTLP exporter is created.
	OTLPEndpoint string
}

// Provider manages metrics exporters: a Prometheus pull endpoint and/or an OTLP push exporter.
// Both are optional and independently controlled by the Config fields passed to New.
type Provider struct {
	meterProvider otelmetric.MeterProvider
	shutdown      func(context.Context) error
}

type internalConfig struct {
	prometheusListener net.Listener
}

// Option configures Provider construction.
type Option func(*internalConfig)

// WithPrometheusListener supplies a pre-bound listener for the Prometheus HTTP server.
// Config.MetricsAddr must be non-empty to enable the server; the provided listener is
// used instead of binding a new one. The caller is responsible for ensuring the listener
// address matches Config.MetricsAddr. Primarily useful in tests to eliminate TOCTOU races.
func WithPrometheusListener(l net.Listener) Option {
	return func(c *internalConfig) {
		c.prometheusListener = l
	}
}

// New creates a Provider from cfg. If neither MetricsAddr nor OTLPEndpoint is set,
// a no-op MeterProvider is returned.
func New(ctx context.Context, cfg Config, opts ...Option) (*Provider, error) {
	icfg := &internalConfig{}
	for _, o := range opts {
		o(icfg)
	}

	var readers []sdkmetric.Reader
	var shutdownFuncs []func(context.Context) error

	if cfg.MetricsAddr != "" {
		reg := prometheus.NewRegistry()
		promReader, err := prometheusexporter.New(prometheusexporter.WithRegisterer(reg))
		if err != nil {
			return nil, fmt.Errorf("create prometheus exporter: %w", err)
		}
		readers = append(readers, promReader)

		l := icfg.prometheusListener
		if l == nil {
			l, err = net.Listen("tcp", cfg.MetricsAddr)
			if err != nil {
				return nil, fmt.Errorf("listen on metrics addr %s: %w", cfg.MetricsAddr, err)
			}
		}

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		srv := &http.Server{Handler: mux}
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "metrics HTTP server error: %v\n", err)
			}
		}()
		shutdownFuncs = append(shutdownFuncs, srv.Shutdown)
	}

	if cfg.OTLPEndpoint != "" {
		otlpExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(cfg.OTLPEndpoint))
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

	sdkOpts := make([]sdkmetric.Option, 0, len(readers))
	for _, r := range readers {
		sdkOpts = append(sdkOpts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(sdkOpts...)
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
