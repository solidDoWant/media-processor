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

func TestPrometheusEndpoint_Enabled(t *testing.T) {
	addr := freeAddr(t)

	p, err := metrics.New(metrics.WithMetricsAddr(addr))
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
	p, err := metrics.New()
	require.NoError(t, err)

	// PrometheusRegisterer must be nil so downstream consumers (e.g.
	// pkg/temporalclient.newMetricsHandler) can detect "metrics off" and
	// short-circuit to a noop rather than collecting into an unreachable registry.
	require.Nil(t, p.PrometheusRegisterer())

	// Shutdown must succeed without error.
	require.NoError(t, p.Shutdown(context.Background()))
}

func TestNewFromEnv_FallsBackToDefaultAddr(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", "")

	// NewFromEnv binds the supplied default when METRICS_ADDR is unset.
	p, shutdown, err := metrics.NewFromEnv(addr)
	require.NoError(t, err)
	t.Cleanup(shutdown)

	require.NotNil(t, p.PrometheusRegisterer())

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/metrics") //nolint:noctx
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNewFromEnv_EmptyDefault_KeepsMetricsDisabled(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")

	// Empty defaultAddr leaves the endpoint disabled when METRICS_ADDR is
	// also unset — useful for tests that want the env path without binding
	// any real port.
	p, shutdown, err := metrics.NewFromEnv("")
	require.NoError(t, err)
	t.Cleanup(shutdown)

	require.Nil(t, p.PrometheusRegisterer())
}

func TestNewFromEnv_MetricsAddr_StartsPrometheusEndpoint(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", addr)

	p, shutdown, err := metrics.NewFromEnv("")
	require.NoError(t, err)
	t.Cleanup(shutdown)
	require.NotNil(t, p)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/metrics") //nolint:noctx
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestWaitForScrape_ReturnsWhenScrapeArrives(t *testing.T) {
	addr := freeAddr(t)

	// Generous configured timeout — the test must not hit it on the happy path.
	p, err := metrics.New(
		metrics.WithMetricsAddr(addr),
		metrics.WithScrapeWaitTimeout(5*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = p.Shutdown(ctx) //nolint:errcheck
	})

	// Issue an initial synchronous scrape before starting WaitForScrape so the
	// notify-buffer is in a known pre-filled state. WaitForScrape's drain step
	// then deterministically clears that buffered tick, leaving the second
	// scrape (issued below) as the only event that can satisfy the wait. This
	// avoids the timing-dependent goroutine-ordering hazard of starting
	// WaitForScrape and the first scrape concurrently.
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/metrics") //nolint:noctx
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	waitErr := make(chan error, 1)

	go func() {
		waitErr <- p.WaitForScrape(t.Context())
	}()

	resp, err = (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/metrics") //nolint:noctx
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

	p, err := metrics.New(
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
	p, err := metrics.New(metrics.WithScrapeWaitTimeout(10 * time.Second))
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

func TestWaitForScrape_DisabledByExplicitZero(t *testing.T) {
	addr := freeAddr(t)

	// Explicit zero must short-circuit the wait rather than fall back to the
	// default 60s. This distinguishes "not provided" (which uses the default)
	// from "explicitly disabled" (which returns immediately).
	p, err := metrics.New(
		metrics.WithMetricsAddr(addr),
		metrics.WithScrapeWaitTimeout(0),
	)
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
	assert.Less(t, elapsed, 500*time.Millisecond, "WaitForScrape should return immediately when timeout is explicitly zero")
}

func TestNewFromEnv_ScrapeWaitTimeout_DisabledViaZeroEnvVar(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", addr)
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "0s")

	p, shutdown, err := metrics.NewFromEnv("")
	require.NoError(t, err)
	t.Cleanup(shutdown)

	start := time.Now()
	err = p.WaitForScrape(t.Context())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond, "METRICS_SCRAPE_WAIT_TIMEOUT=0s should disable the gate")
}

func TestNewFromEnv_ScrapeWaitTimeout_AppliedFromEnv(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv("METRICS_ADDR", addr)
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "100ms")

	p, shutdown, err := metrics.NewFromEnv("")
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
	t.Setenv("METRICS_SCRAPE_WAIT_TIMEOUT", "not-a-duration")

	_, _, err := metrics.NewFromEnv("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "METRICS_SCRAPE_WAIT_TIMEOUT")
}
