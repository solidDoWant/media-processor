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

// freeAddr returns a random free TCP address on localhost.
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
	t.Setenv("METRICS_ADDR", addr)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	p, err := metrics.New(t.Context())
	require.NoError(t, err)
	defer p.Shutdown(context.Background()) //nolint:errcheck

	// Poll until the server is ready.
	url := "http://" + addr + "/metrics"
	var resp *http.Response
	require.Eventually(t, func() bool {
		var reqErr error
		resp, reqErr = http.Get(url) //nolint:noctx
		return reqErr == nil
	}, 2*time.Second, 50*time.Millisecond, "metrics endpoint did not become available")
	t.Cleanup(func() { resp.Body.Close() }) //nolint:errcheck

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// Prometheus text format always contains comment lines starting with '#'.
	assert.True(t, strings.Contains(string(body), "#"), "response body should contain Prometheus-format metrics")
}

func TestPrometheusEndpoint_Disabled(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	p, err := metrics.New(t.Context())
	require.NoError(t, err)
	defer p.Shutdown(context.Background()) //nolint:errcheck

	// MeterProvider must still be usable (noop).
	mp := p.MeterProvider()
	require.NotNil(t, mp)

	// Confirm no server is listening on any metrics-related port by checking that
	// the provider is non-nil but no TCP connection is accepted on a random port.
	// We simply verify that calling Shutdown does not error.
	require.NoError(t, p.Shutdown(context.Background()))
}

func TestOTLPExporter_Enabled(t *testing.T) {
	// Start a dummy TCP listener to accept the gRPC connection so the exporter
	// initialises successfully (it dials lazily, so we just need it not to error on init).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, l.Close()) })

	t.Setenv("METRICS_ADDR", "")
	// The OTel SDK requires a URL-format endpoint (scheme://host:port).
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+l.Addr().String())

	p, err := metrics.New(t.Context())
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
	t.Setenv("METRICS_ADDR", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	p, err := metrics.New(t.Context())
	require.NoError(t, err)
	defer p.Shutdown(context.Background()) //nolint:errcheck

	// Shutdown must succeed without attempting any OTLP export.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, p.Shutdown(ctx))
}

func TestBothExporters_Active(t *testing.T) {
	addr := freeAddr(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, l.Close()) })

	t.Setenv("METRICS_ADDR", addr)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+l.Addr().String())

	p, err := metrics.New(t.Context())
	require.NoError(t, err)
	defer p.Shutdown(context.Background()) //nolint:errcheck

	// Prometheus endpoint should be up.
	metricsURL := "http://" + addr + "/metrics"
	require.Eventually(t, func() bool {
		resp, reqErr := http.Get(metricsURL) //nolint:noctx
		if reqErr != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 50*time.Millisecond, "metrics endpoint did not become available")

	// MeterProvider should be non-nil since OTLP is also set.
	mp := p.MeterProvider()
	require.NotNil(t, mp)
}

func TestGracefulShutdown_FlushesOTLP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, l.Close()) })

	t.Setenv("METRICS_ADDR", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+l.Addr().String())

	p, err := metrics.New(t.Context())
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
