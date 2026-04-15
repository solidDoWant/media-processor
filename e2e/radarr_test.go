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

// TestRadarrHappyPath pushes a Big Buck Bunny release to Radarr, waits for the
// pipeline to transcode it and notify Radarr, then verifies the movie is
// imported, the original .mp4 source file has been deleted, and the output
// file has the expected media properties.
func TestRadarrHappyPath(t *testing.T) {
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
		t.Logf("Radarr push response: %s", pushResp)
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

	// Source .mp4 must be deleted by the worker's cleanup step.
	sourceMp4 := filepath.Join(downloadsDir, "radarr", releaseTitle+".mp4")
	_, statErr := os.Stat(sourceMp4)

	assert.ErrorIs(t, statErr, os.ErrNotExist, "source .mp4 should have been deleted after import")

	// .mkv must exist somewhere under the Radarr library directory.
	mkvPath := findMKV(t, filepath.Join(processedDir, "radarr-library"))
	require.NotEmpty(t, mkvPath, "expected .mkv in radarr-library after import")

	// Verify output file properties.
	info := probeOutputFile(t, mkvPath)
	assert.Contains(t, info.formatName, "matroska", "output container should be Matroska")
	assert.Equal(t, "hevc", info.videoCodec, "output video codec should be H.265")
	assert.Greater(t, info.durationSec, 300.0, "output duration should be at least 5 minutes")
}
