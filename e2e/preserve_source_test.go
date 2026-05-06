//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreserveSourceSentinel verifies the sentinel-file mechanism for the
// preserve-source watch mapping. It writes a plain-text file (not valid media),
// waits for the probe step to reject it and the record_invalid step to write the
// sentinel, then confirms the watcher does not dispatch it a second time.
func TestPreserveSourceSentinel(t *testing.T) {
	// Run alongside the radarr and sonarr happy paths so the worker pools see
	// continuous work and do not idle-exit between tests. With
	// WORKER_IDLE_EXIT_AFTER=5s in compose, a sequential run would let the
	// workers terminate during the inter-test gap and break later tests.
	t.Parallel()

	srcFile := filepath.Join(downloadsDir, "preserve-source", "not-a-video.txt")
	sentinelFile := filepath.Join(downloadsDir, "preserve-source", ".not-a-video.txt.done")

	require.NoError(t, os.WriteFile(srcFile, []byte("not media"), 0o644), "write dummy source file")

	// Wait for the worker to process the file (probe rejects it, record_invalid
	// writes the sentinel).
	sentinelCtx, sentinelCancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer sentinelCancel()

	err := pollUntil(sentinelCtx, 10*time.Second, func() error {
		_, statErr := os.Stat(sentinelFile)
		return statErr
	})
	require.NoError(t, err, "sentinel file was not created within the timeout")

	// Capture the current dispatch and scan counts for this mapping.
	filter := map[string]string{"mapping_name": "preserve-source"}
	dispatchCount := fetchMetrics(t, watcherMetricsAddr).sum("watcher_dispatches_total", filter)
	scanCount := fetchMetrics(t, watcherMetricsAddr).sum("watcher_scans_total", filter)

	// Wait for at least one more complete scan after the sentinel exists.
	scanCtx, scanCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer scanCancel()

	err = pollUntil(scanCtx, 5*time.Second, func() error {
		current := fetchMetrics(t, watcherMetricsAddr).sum("watcher_scans_total", filter)
		if current > scanCount {
			return nil
		}

		return fmt.Errorf("scan count not yet advanced (current=%.0f, baseline=%.0f)", current, scanCount)
	})
	require.NoError(t, err, "watcher did not complete a follow-up scan within the timeout")

	// Dispatch count must not have increased — the sentinel prevented re-dispatch.
	newDispatchCount := fetchMetrics(t, watcherMetricsAddr).sum("watcher_dispatches_total", filter)
	assert.Equal(t, dispatchCount, newDispatchCount, "watcher should not re-dispatch a file whose sentinel exists")
}
