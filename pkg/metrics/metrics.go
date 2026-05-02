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
	prometheusexporter "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// DefaultScrapeWaitTimeout is the default upper bound for Provider.WaitForScrape
// when no explicit timeout is configured via WithScrapeWaitTimeout or
// METRICS_SCRAPE_WAIT_TIMEOUT.
const DefaultScrapeWaitTimeout = 60 * time.Second

// Provider manages an optional Prometheus pull endpoint, controlled by the
// options passed to New.
type Provider struct {
	meterProvider     otelmetric.MeterProvider
	promRegistry      *prometheus.Registry
	shutdown          func(context.Context) error
	scrapeNotify      chan struct{}
	scrapeWaitTimeout time.Duration
}

type config struct {
	metricsAddr       string
	scrapeWaitTimeout *time.Duration
}

// Option configures Provider construction.
type Option func(*config)

// WithMetricsAddr sets the TCP address for the Prometheus /metrics HTTP server (e.g. ":9090").
// When not supplied, no HTTP server is started.
func WithMetricsAddr(addr string) Option {
	return func(c *config) { c.metricsAddr = addr }
}

// WithScrapeWaitTimeout sets the upper bound for Provider.WaitForScrape.
// When this option is not supplied at all, DefaultScrapeWaitTimeout is used.
// A non-positive value (e.g. 0) disables the gate: WaitForScrape returns nil
// immediately rather than blocking. Distinguishing "not provided" from
// "explicitly zero" lets operators short-circuit the wait via
// METRICS_SCRAPE_WAIT_TIMEOUT=0s without falling back to the default.
func WithScrapeWaitTimeout(d time.Duration) Option {
	return func(c *config) { c.scrapeWaitTimeout = &d }
}

// New creates a Provider. If WithMetricsAddr is not supplied, a no-op
// MeterProvider is returned.
func New(ctx context.Context, opts ...Option) (*Provider, error) {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	// Resolve the configured timeout. nil → not provided → use the default.
	// A non-nil value (including zero or negative) is honored as-is; WaitForScrape
	// treats a non-positive timeout as "gate disabled" and returns nil immediately.
	scrapeWaitTimeout := DefaultScrapeWaitTimeout
	if cfg.scrapeWaitTimeout != nil {
		scrapeWaitTimeout = *cfg.scrapeWaitTimeout
	}

	var (
		readers       []sdkmetric.Reader
		shutdownFuncs []func(context.Context) error
		promRegistry  *prometheus.Registry
		scrapeNotify  chan struct{}
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

		// Buffered notify channel signals that a /metrics scrape has been served.
		// Buffer size 1 is enough: WaitForScrape drains any pending tick before
		// it blocks, so only fresh post-drain scrapes can satisfy the wait.
		scrapeNotify = make(chan struct{}, 1)
		promHandler := promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{})

		mux := http.NewServeMux()
		mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			promHandler.ServeHTTP(w, r)

			select {
			case scrapeNotify <- struct{}{}:
			default:
			}
		}))
		srv := &http.Server{Handler: mux}

		go func() {
			if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "metrics HTTP server error: %v\n", err)
			}
		}()

		shutdownFuncs = append(shutdownFuncs, srv.Shutdown)
	}

	if len(readers) == 0 {
		return &Provider{
			meterProvider:     noop.NewMeterProvider(),
			promRegistry:      promRegistry,
			shutdown:          func(context.Context) error { return nil },
			scrapeNotify:      scrapeNotify,
			scrapeWaitTimeout: scrapeWaitTimeout,
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
			// Shut down in reverse (LIFO) order so the OTel MeterProvider is
			// flushed before the Prometheus HTTP server is stopped.
			for i := len(shutdownFuncs) - 1; i >= 0; i-- {
				errs = append(errs, shutdownFuncs[i](ctx))
			}

			return errors.Join(errs...)
		},
		scrapeNotify:      scrapeNotify,
		scrapeWaitTimeout: scrapeWaitTimeout,
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

// WaitForScrape blocks until the /metrics HTTP handler serves a request after
// this call begins, the configured scrape-wait timeout elapses, or ctx is
// cancelled (whichever happens first). It returns nil immediately when no
// Prometheus HTTP server is configured or when the configured timeout is
// non-positive (gate explicitly disabled).
//
// Intended use: invoke after process drain but before exporter shutdown so
// Prometheus has the opportunity to collect a final scrape covering
// end-of-lifecycle metrics. The HTTP server is left running for the duration
// of the wait. Callers should pass a non-cancelled parent context (typically
// context.Background()) — passing a SIGTERM-cancelled context defeats the gate.
func (p *Provider) WaitForScrape(ctx context.Context) error {
	if p.scrapeNotify == nil {
		return nil
	}

	if p.scrapeWaitTimeout <= 0 {
		return nil
	}

	// Drain any tick buffered by a scrape that arrived before this call so
	// only a fresh post-drain scrape can satisfy the wait.
	select {
	case <-p.scrapeNotify:
	default:
	}

	waitCtx, cancel := context.WithTimeout(ctx, p.scrapeWaitTimeout)
	defer cancel()

	select {
	case <-p.scrapeNotify:
		return nil
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

// NewFromEnv creates a Provider using standard environment variables.
// METRICS_ADDR enables the Prometheus /metrics pull endpoint.
// METRICS_SCRAPE_WAIT_TIMEOUT (Go duration string) bounds Provider.WaitForScrape.
// If METRICS_ADDR is not set, a no-op Provider is returned.
//
// The returned shutdown func must be deferred by the caller. It shuts down all
// exporters with a 10-second deadline and writes any error to stderr.
func NewFromEnv(ctx context.Context) (*Provider, func(), error) {
	var opts []Option
	if addr := os.Getenv("METRICS_ADDR"); addr != "" {
		opts = append(opts, WithMetricsAddr(addr))
	}

	if raw := os.Getenv("METRICS_SCRAPE_WAIT_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, func() {}, fmt.Errorf("METRICS_SCRAPE_WAIT_TIMEOUT must be a valid duration (got %q): %w", raw, err)
		}

		opts = append(opts, WithScrapeWaitTimeout(d))
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
