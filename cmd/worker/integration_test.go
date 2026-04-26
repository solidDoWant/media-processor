//go:build integration

package main

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

// TestWorkerConnectsToHatchet verifies that the worker successfully connects to a
// running Hatchet server and stays running until signalled to stop.
//
// Requires a running Hatchet server. Run `make hatchet-up` and `source .env.hatchet`
// before executing these tests, or use `make test-integration`.
func TestWorkerConnectsToHatchet(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	// Provide the env vars required by run(). Neither Radarr nor Sonarr is exercised
	// by this test — dummy values are sufficient.
	t.Setenv("RADARR_URL", "http://localhost:9999")
	t.Setenv("RADARR_API_KEY", "test-key")
	t.Setenv("SONARR_URL", "http://localhost:9998")
	t.Setenv("SONARR_API_KEY", "test-key")
	t.Setenv("HEALTH_ADDR", freeAddr(t))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- run(ctx)
	}()

	// If the worker fails to connect, run() returns quickly with an error.
	// If it connects successfully, it blocks until the context is cancelled.
	// Wait long enough to distinguish the two cases.
	const connectionWindow = 10 * time.Second
	timer := time.NewTimer(connectionWindow)
	defer timer.Stop()

	select {
	case err := <-done:
		require.NoError(t, err, "worker exited before context was cancelled — connection likely failed")
	case <-timer.C:
		// Worker is still running after the window — connection succeeded.
	}

	cancel()
	require.NoError(t, <-done, "worker should shut down cleanly")
}
