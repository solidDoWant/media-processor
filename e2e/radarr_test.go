//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRadarrHappyPath pushes a Big Buck Bunny release to Radarr, waits for the
// pipeline to transcode it and notify Radarr, then verifies the movie is
// imported, the original .mp4 source file has been deleted, and the output
// file has the expected media properties.
func TestRadarrHappyPath(t *testing.T) {
	// Run alongside the sonarr happy path so the transcode worker pool sees
	// back-to-back transcodes and does not idle-exit between tests. The
	// transcode pool is configured with a 15s WORKER_IDLE_EXIT_AFTER in
	// compose; a sequential run would let it drain after the radarr transcode
	// and leave the sonarr transcode without a worker to pick it up.
	t.Parallel()

	radarr := newArrClient(radarrBase, radarrAPIKey)

	const releaseTitle = "Big.Buck.Bunny.2008.1080p.WEB-DL"

	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%040x&dn=%s", 1, releaseTitle)

	var pushResp []json.RawMessage

	require.NoError(t, radarr.post(t.Context(), "/api/v3/release/push", map[string]any{
		"title":       releaseTitle,
		"downloadUrl": magnet,
		"protocol":    "Torrent",
		"publishDate": time.Now().UTC().Format(time.RFC3339),
		"indexer":     "e2e-test",
		"size":        700_000_000,
		"movieId":     radarrMovieID,
	}, &pushResp), "push release to Radarr")

	if len(pushResp) > 0 {
		if respJSON, marshalErr := json.Marshal(pushResp); marshalErr != nil {
			t.Logf("Radarr push response (failed to marshal as JSON: %v): %+v", marshalErr, pushResp)
		} else {
			t.Logf("Radarr push response: %s", respJSON)
		}
	}

	// Poll until Radarr has imported the movie (hasFile=true).
	// The timeout is set generously to accommodate a full H.265 software encode
	// of the BBB fixture on machines without hardware acceleration.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Minute)
	defer cancel()

	err := pollUntil(ctx, 10*time.Second, func() error {
		var movie struct {
			HasFile bool `json:"hasFile"`
		}

		if err := radarr.get(ctx, fmt.Sprintf("/api/v3/movie/%d", radarrMovieID), &movie); err != nil {
			return err
		}

		if !movie.HasFile {
			return fmt.Errorf("movie not yet imported")
		}

		return nil
	})
	require.NoError(t, err, "Radarr did not import the movie within the timeout")

	// preserveSource=false (default): source .mp4 must be deleted by the
	// worker's cleanup step. Radarr's import-detection poll can fire before
	// the workflow has dispatched its post-Notify Cleanup activity to the
	// rest worker pool, so wait briefly for the deletion rather than asserting
	// it instantly.
	sourceMp4 := filepath.Join(downloadsDir, "radarr", releaseTitle, releaseTitle+".mp4")
	releaseDir := filepath.Join(downloadsDir, "radarr", releaseTitle)

	assertPathRemoved(t, sourceMp4, "source .mp4 should have been deleted after import (preserveSource=false)")

	// retainEmptyDirectories=false (default): the release subdirectory must
	// be pruned once it becomes empty after source-file deletion.
	assertPathRemoved(t, releaseDir, "release subdirectory should have been pruned after source deletion (retainEmptyDirectories=false)")

	// .mkv must exist somewhere under the Radarr library directory.
	mkvPath := findMKV(t, filepath.Join(processedDir, "radarr-library"))
	require.NotEmpty(t, mkvPath, "expected .mkv in radarr-library after import")

	// Verify output file properties.
	info := probeOutputFile(t, mkvPath)
	assert.Contains(t, info.formatName, "matroska", "output container should be Matroska")
	assert.Equal(t, "hevc", info.videoCodec, "output video codec should be H.265")
	assert.Greater(t, info.durationSec, 300.0, "output duration should be at least 5 minutes")

	assertRadarrPipelineMetrics(t)
}

func assertRadarrPipelineMetrics(t *testing.T) {
	t.Helper()

	// The watcher counters are updated synchronously from the scan goroutine.
	// On the worker side, application metrics flush through the tally→Prometheus
	// pipeline on a 1-second interval; the SDK records temporal_workflow_endtoend_latency
	// only when the workflow's close command fires (after Notify + Cleanup), so it
	// lands strictly later than the per-mapping transcode_duration. Poll until BOTH:
	//   - the per-mapping transcode_duration histogram (proves THIS workflow
	//     reached transcode), and
	//   - the SDK end-to-end histogram (proves SOME MediaWorkflow has fully
	//     closed; replaces the removed media_workflow_total_duration_seconds)
	// are present, so the snapshot is internally consistent for the per-metric
	// assertions below.
	filter := map[string]string{"mapping_name": "radarr"}
	sdkFilter := map[string]string{"workflow_type": "Media"}

	var workerSeries metricSeries

	require.Eventually(t, func() bool {
		workerSeries = fetchAllWorkerMetrics(t)

		return workerSeries.sum("media_workflow_transcode_duration_seconds_count", filter) >= 1 &&
			workerSeries.sum("temporal_workflow_endtoend_latency_seconds_count", sdkFilter) >= 1
	}, 30*time.Second, 500*time.Millisecond,
		"expected the worker pools to record one transcode-duration observation for the radarr mapping AND one Media-workflow end-to-end latency observation")

	watcherSeries := fetchMetrics(t, watcherMetricsAddr)
	assert.Greater(t, watcherSeries.sum("watcher_scans_total", filter), 0.0,
		"watcher should have completed at least one scan of the radarr mapping")
	assert.Greater(t, watcherSeries.sum("watcher_files_discovered_total", filter), 0.0,
		"watcher should have discovered the radarr source file")
	assert.Greater(t, watcherSeries.sum("watcher_dispatches_total", filter), 0.0,
		"watcher should have dispatched the radarr workflow to Temporal")

	assert.Greater(t, workerSeries.sum("media_workflow_destination_file_size_bytes_sum", filter), 0.0,
		"worker should have recorded a positive destination-file-size observation")
}
