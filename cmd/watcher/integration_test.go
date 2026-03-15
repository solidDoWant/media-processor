//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWatcherConnectsToHatchet verifies that the watcher successfully connects to a
// running Hatchet server given a valid config file.
//
// Requires a running Hatchet server. Run `make hatchet-up` and `source .env.hatchet`
// before executing these tests, or use `make test-integration`.
func TestWatcherConnectsToHatchet(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	cfgPath := writeTempConfig(t, "watches: []")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfgPath)
	}()

	// If the watcher fails to connect, run() returns quickly with an error.
	// If it connects successfully, it blocks until the context is cancelled.
	const connectionWindow = 10 * time.Second
	timer := time.NewTimer(connectionWindow)
	defer timer.Stop()

	select {
	case err := <-done:
		require.NoError(t, err, "watcher exited before context was cancelled — connection likely failed")
	case <-timer.C:
		// Watcher is still running after the window — connection succeeded.
	}

	cancel()
	require.NoError(t, <-done, "watcher should shut down cleanly")
}
