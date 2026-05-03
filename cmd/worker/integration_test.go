//go:build integration

package main

import (
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

// TestWorkerConnectsToTemporal verifies that the worker successfully connects
// to a running Temporal server and stays running until signalled to stop.
//
// Requires a running Temporal server reachable at TEMPORAL_ADDRESS.
func TestWorkerConnectsToTemporal(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	if os.Getenv("TEMPORAL_TASK_QUEUE") == "" {
		t.Setenv("TEMPORAL_TASK_QUEUE", "media-processor-test")
	}

	// Provide the env vars required by run(). Neither Radarr nor Sonarr is
	// exercised by this test — dummy values are sufficient.
	t.Setenv("RADARR_URL", "http://localhost:9999")
	t.Setenv("RADARR_API_KEY", "test-key")
	t.Setenv("SONARR_URL", "http://localhost:9998")
	t.Setenv("SONARR_API_KEY", "test-key")
	t.Setenv("HEALTH_ADDR", freeAddr(t))

	interrupt := make(chan interface{}, 1)
	done := make(chan error, 1)

	go func() {
		done <- run(t.Context(), interrupt)
	}()

	// If the worker fails to connect, run() returns quickly with an error.
	// If it connects successfully, it blocks until the interrupt channel
	// receives. Wait long enough to distinguish the two cases.
	const connectionWindow = 10 * time.Second

	timer := time.NewTimer(connectionWindow)
	defer timer.Stop()

	select {
	case err := <-done:
		require.NoError(t, err, "worker exited before interrupt was sent — connection likely failed")
	case <-timer.C:
		// Worker is still running after the window — connection succeeded.
	}

	interrupt <- struct{}{}

	require.NoError(t, <-done, "worker should shut down cleanly")
}
