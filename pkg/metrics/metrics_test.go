package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/metrics"
)

// freeAddr returns a local TCP address with an available port.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

// bindListener opens a TCP listener on a random port and registers cleanup.
// The cleanup silently ignores close errors because the listener may already be
// closed by the HTTP server's Shutdown when a Provider is active.
func bindListener(t *testing.T) net.Listener {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	return l
}

func TestPrometheusEndpoint_Enabled(t *testing.T) {
	addr := freeAddr(t)

	p, err := metrics.New(t.Context(), metrics.WithMetricsAddr(addr))
	require.NoError(t, err)

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = p.Shutdown(ctx) //nolint:errcheck
	}()

	url := "http://" + addr + "/metrics"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url) //nolint:noctx
	require.NoError(t, err)
	// defer (not t.Cleanup) so resp.Body closes before the Shutdown defer above (LIFO).
	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Verify the response Content-Type is Prometheus text exposition format.
	contentType := resp.Header.Get("Content-Type")
	assert.True(t, strings.HasPrefix(contentType, "text/plain"), "metrics endpoint should return Prometheus text exposition format, got: %s", contentType)

	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
}

func TestPrometheusEndpoint_Disabled(t *testing.T) {
	p, err := metrics.New(t.Context())
	require.NoError(t, err)

	// MeterProvider must still be usable (noop).
	mp := p.MeterProvider()
	require.NotNil(t, mp)

	// Shutdown must succeed without error.
	require.NoError(t, p.Shutdown(context.Background()))
}

func TestOTLPExporter_Enabled(t *testing.T) {
	// Start a dummy TCP listener to accept the gRPC connection so the exporter
	// initialises successfully (it dials lazily, so we just need it not to error on init).
	l := bindListener(t)
	// The OTel SDK requires a URL-format endpoint (scheme://host:port).
	p, err := metrics.New(t.Context(), metrics.WithOTLPEndpoint("http://"+l.Addr().String()))
	require.NoError(t, err)

	mp := p.MeterProvider()
	require.NotNil(t, mp, "MeterProvider should be non-nil when OTLP endpoint is set")

	// Shutdown should complete within the deadline (even if flushing fails because
	// the listener is not a real gRPC server — the important thing is it is called).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = p.Shutdown(ctx) // error tolerated: no real gRPC server behind the listener
}

func TestOTLPExporter_Disabled(t *testing.T) {
	p, err := metrics.New(t.Context())
	require.NoError(t, err)

	// Shutdown must succeed without attempting any OTLP export.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, p.Shutdown(ctx))
}

func TestBothExporters_Active(t *testing.T) {
	promAddr := freeAddr(t)
	otlpListener := bindListener(t)

	p, err := metrics.New(t.Context(),
		metrics.WithMetricsAddr(promAddr),
		metrics.WithOTLPEndpoint("http://"+otlpListener.Addr().String()),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = p.Shutdown(ctx) //nolint:errcheck
	})

	// Prometheus endpoint should respond.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + promAddr + "/metrics") //nolint:noctx
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// MeterProvider should be non-nil since OTLP is also set.
	mp := p.MeterProvider()
	require.NotNil(t, mp)
}

func TestNewFromEnv_NoEnvVars_ReturnsNoopProvider(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	p, shutdown, err := metrics.NewFromEnv(t.Context())
	require.NoError(t, err)
	t.Cleanup(shutdown)
	require.NotNil(t, p.MeterProvider())
}

func TestNewFromEnv_MetricsAddr_StartsPrometheusEndpoint(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", addr)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	p, shutdown, err := metrics.NewFromEnv(t.Context())
	require.NoError(t, err)
	t.Cleanup(shutdown)
	require.NotNil(t, p)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/metrics") //nolint:noctx
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNewFromEnv_OTLPEndpoint_CreatesProvider(t *testing.T) {
	l := bindListener(t)
	t.Setenv("METRICS_ADDR", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+l.Addr().String())

	// Use p.Shutdown with a short deadline rather than the fixed-10s shutdown func
	// so the test does not block for the full flush timeout against a dummy listener.
	p, _, err := metrics.NewFromEnv(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		_ = p.Shutdown(ctx)
	})
	require.NotNil(t, p.MeterProvider())
}

func TestWaitForScrape_ReturnsWhenScrapeArrives(t *testing.T) {
	addr := freeAddr(t)

	// Generous configured timeout — the test must not hit it on the happy path.
	p, err := metrics.New(t.Context(),
		metrics.WithMetricsAddr(addr),
		metrics.WithScrapeWaitTimeout(5*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = p.Shutdown(ctx) //nolint:errcheck
	})

	// Run WaitForScrape in a goroutine and trigger a scrape.
	waitErr := make(chan error, 1)

	go func() {
		waitErr <- p.WaitForScrape(t.Context())
	}()

	// Small delay so WaitForScrape has time to drain and enter the select before
	// the scrape arrives. Without this, the scrape could fill the buffer before
	// the drain runs and then be cleared. The post-drain select would still
	// observe the next scrape, but we want the test to verify the explicit
	// "scrape arrives during the wait" path.
	time.Sleep(50 * time.Millisecond)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/metrics") //nolint:noctx
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	select {
	case err := <-waitErr:
		require.NoError(t, err, "WaitForScrape should return nil when a scrape arrives")
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForScrape did not return after a scrape was served")
	}
}

func TestWaitForScrape_HonorsTimeout(t *testing.T) {
	addr := freeAddr(t)

	p, err := metrics.New(t.Context(),
		metrics.WithMetricsAddr(addr),
		metrics.WithScrapeWaitTimeout(100*time.Millisecond),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = p.Shutdown(ctx) //nolint:errcheck
	})

	start := time.Now()
	err = p.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.Error(t, err, "WaitForScrape should return an error when no scrape arrives before the timeout")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	// Sanity-bound the elapsed time so we know the timeout actually fired
	// (rather than ctx being cancelled by some unrelated cause).
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	assert.Less(t, elapsed, 2*time.Second, "WaitForScrape should return shortly after the configured timeout")
}

func TestWaitForScrape_NoOpWhenNoHTTPServer(t *testing.T) {
	// No metrics address configured → no Prometheus HTTP server, so WaitForScrape
	// must return nil immediately rather than blocking on a non-existent gate.
	p, err := metrics.New(t.Context(), metrics.WithScrapeWaitTimeout(10*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		_ = p.Shutdown(ctx) //nolint:errcheck
	})

	start := time.Now()
	err = p.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 1*time.Second, "WaitForScrape should return immediately when no HTTP server is configured")
}

func TestNewFromEnv_ScrapeWaitTimeout_AppliedFromEnv(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", addr)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "100ms")

	p, shutdown, err := metrics.NewFromEnv(t.Context())
	require.NoError(t, err)
	t.Cleanup(shutdown)

	start := time.Now()
	err = p.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 2*time.Second, "WaitForScrape should honor the env-configured 100ms timeout")
}

func TestNewFromEnv_ScrapeWaitTimeout_InvalidDurationErrors(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "not-a-duration")

	_, _, err := metrics.NewFromEnv(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "METRICS_SCRAPE_WAIT_TIMEOUT")
}

func TestGracefulShutdown_FlushesOTLP(t *testing.T) {
	l := bindListener(t)

	p, err := metrics.New(t.Context(), metrics.WithOTLPEndpoint("http://"+l.Addr().String()))
	require.NoError(t, err)

	// Shutdown must complete (the MeterProvider.Shutdown must be called, attempting to
	// flush buffered metrics). An error from the remote end is tolerated since the
	// listener is not a real gRPC server. The overall test deadline is generous so
	// the gRPC connection timeout can expire before we declare failure.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()

	shutdownDone := make(chan struct{})

	go func() {
		defer close(shutdownDone)

		_ = p.Shutdown(shutdownCtx)
	}()

	select {
	case <-shutdownDone:
		// passed: Shutdown completed (with or without error from the dummy server)
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not complete within 10 seconds")
	}
}
