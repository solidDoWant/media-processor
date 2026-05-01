//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSonarrHappyPath pushes an S01E01 release of Colonel Bleep (TVDB 254376)
// to Sonarr, waits for the pipeline to transcode it and notify Sonarr, then
// verifies an episode file was imported, the original .mp4 source file has
// been deleted, and the output file has the expected media properties.
//
// Colonel Bleep (1957) is a public-domain animated series (copyright lapsed
// without renewal in 1985) with 6-minute episodes. The BBB fixture at ~9:56
// exceeds Sonarr's 50%-of-episode-runtime sample check (threshold: 3 min).
func TestSonarrHappyPath(t *testing.T) {
	sonarr := newArrClient(sonarrBase, sonarrAPIKey)

	// Include the year so Sonarr's title parser resolves "Colonel Bleep" with
	// year 1957 and matches the library entry stored as "Colonel Bleep (1957)".
	const releaseTitle = "Colonel.Bleep.1957.S01E01.1080p.WEB-DL"

	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%040x&dn=%s", 2, releaseTitle)

	var sonarrPushResp []json.RawMessage

	require.NoError(t, sonarr.post(t.Context(), "/api/v3/release/push", map[string]any{
		"title":              releaseTitle,
		"downloadUrl":        magnet,
		"protocol":           "Torrent",
		"publishDate":        time.Now().UTC().Format(time.RFC3339),
		"indexer":            "e2e-test",
		"size":               700_000_000,
		"mappedSeriesId":     sonarrSeriesID,
		"mappedSeasonNumber": 1,
		"mappedEpisodeIds":   []int{sonarrEpisodeID},
	}, &sonarrPushResp), "push release to Sonarr")

	if len(sonarrPushResp) > 0 {
		if respJSON, marshalErr := json.Marshal(sonarrPushResp); marshalErr != nil {
			t.Logf("Sonarr push response (failed to marshal as JSON: %v): %+v", marshalErr, sonarrPushResp)
		} else {
			t.Logf("Sonarr push response: %s", respJSON)
		}
	}

	// Poll until Sonarr has at least one imported episode file.
	// The timeout is set generously to accommodate a full H.265 software encode
	// of the BBB fixture on machines without hardware acceleration.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Minute)
	defer cancel()

	err := pollUntil(ctx, 10*time.Second, func() error {
		var files []struct {
			ID int `json:"id"`
		}

		if err := sonarr.get(ctx, fmt.Sprintf("/api/v3/episodefile?seriesId=%d", sonarrSeriesID), &files); err != nil {
			return err
		}

		if len(files) == 0 {
			return fmt.Errorf("no episode files imported yet")
		}

		return nil
	})
	require.NoError(t, err, "Sonarr did not import any episode file within the timeout")

	// preserveSource=false (default): source .mp4 must be deleted by the worker's cleanup step.
	sourceMp4 := filepath.Join(downloadsDir, "sonarr", releaseTitle, releaseTitle+".mp4")
	_, statErr := os.Stat(sourceMp4)

	assert.ErrorIs(t, statErr, os.ErrNotExist, "source .mp4 should have been deleted after import (preserveSource=false)")

	// retainEmptyDirectories=false (default): the release subdirectory must be pruned once
	// it becomes empty after source-file deletion.
	releaseDir := filepath.Join(downloadsDir, "sonarr", releaseTitle)
	_, dirStatErr := os.Stat(releaseDir)

	assert.ErrorIs(t, dirStatErr, os.ErrNotExist, "release subdirectory should have been pruned after source deletion (retainEmptyDirectories=false)")

	// .mkv must exist somewhere under the Sonarr library directory.
	mkvPath := findMKV(t, filepath.Join(processedDir, "sonarr-library"))
	require.NotEmpty(t, mkvPath, "expected .mkv in sonarr-library after import")

	// Verify output file properties.
	info := probeOutputFile(t, mkvPath)
	assert.Contains(t, info.formatName, "matroska", "output container should be Matroska")
	assert.Equal(t, "hevc", info.videoCodec, "output video codec should be H.265")
	assert.Greater(t, info.durationSec, 300.0, "output duration should be at least 5 minutes")

	assertSonarrPipelineMetrics(t)
}

func assertSonarrPipelineMetrics(t *testing.T) {
	t.Helper()

	// The watcher counters are updated synchronously from the scan goroutine;
	// the worker's record_metrics task runs after cleanup, so poll until it lands.
	filter := map[string]string{"mapping_name": "sonarr"}

	var workerSeries metricSeries

	require.Eventually(t, func() bool {
		workerSeries = fetchMetrics(t, workerMetricsAddr)
		return workerSeries.sum("media_workflow_total_duration_seconds_count", filter) >= 1
	}, 30*time.Second, 500*time.Millisecond,
		"expected worker to record at least one completed media_workflow run for the sonarr mapping")

	watcherSeries := fetchMetrics(t, watcherMetricsAddr)
	assert.Greater(t, watcherSeries.sum("watcher_scans_total", filter), 0.0,
		"watcher should have completed at least one scan of the sonarr mapping")
	assert.Greater(t, watcherSeries.sum("watcher_files_discovered_total", filter), 0.0,
		"watcher should have discovered the sonarr source file")
	assert.Greater(t, watcherSeries.sum("watcher_dispatches_total", filter), 0.0,
		"watcher should have dispatched the sonarr workflow to Temporal")

	assert.GreaterOrEqual(t, workerSeries.sum("media_workflow_transcode_duration_seconds_count", filter), 1.0,
		"worker should have recorded the sonarr transcode duration")
	assert.Greater(t, workerSeries.sum("media_workflow_destination_file_size_bytes_sum", filter), 0.0,
		"worker should have recorded a positive destination-file-size observation")
}
