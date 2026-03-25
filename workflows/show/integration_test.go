//go:build integration

package show

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// startShowWorker creates a Hatchet worker with the given ShowWorkflow and starts it.
// The worker is automatically stopped via t.Cleanup. The call blocks briefly to allow
// the worker to register its workflows with Hatchet before dispatching runs.
func startShowWorker(t *testing.T, client *hatchet.Client, wf *hatchet.Workflow) {
	t.Helper()
	worker, err := client.NewWorker("test-show-worker", hatchet.WithWorkflows(wf))
	require.NoError(t, err, "create Hatchet worker")

	cleanup, err := worker.Start()
	require.NoError(t, err, "start worker")
	t.Cleanup(func() { _ = cleanup() })

	// Allow time for the worker to register its workflow definitions with the Hatchet server.
	time.Sleep(3 * time.Second)
}

func TestShowWorkflow_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	sonarrStub := &stubEpisodeLibrary{episode: medialib.Episode{ID: 1, SeriesID: 42, SeriesTitle: "Test Show"}}
	wf := NewShowWorkflow(client, ShowWorkflowConfig{OutputDir: outputDir}, sonarrStub, &webhook.Client{})
	startShowWorker(t, client, wf)

	ctx := t.Context()
	_, err = wf.Run(ctx, ShowInput{FilePath: inputPath})
	require.NoError(t, err)

	// Transcoded output file must exist in the output directory with .mkv extension.
	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	// Original source file must be deleted by the cleanup step.
	_, statErr = os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted by cleanup step")
}

func TestShowWorkflow_RefreshSeriesIsCalledAfterTranscode(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	sonarrStub := &stubEpisodeLibrary{episode: medialib.Episode{ID: 1, SeriesID: 42, SeriesTitle: "Test Show"}}
	wf := NewShowWorkflow(client, ShowWorkflowConfig{OutputDir: outputDir}, sonarrStub, &webhook.Client{})
	startShowWorker(t, client, wf)

	ctx := t.Context()
	_, err = wf.Run(ctx, ShowInput{FilePath: inputPath})
	require.NoError(t, err)

	// RefreshSeries must have been called with the correct series ID.
	assert.Equal(t, []int64{42}, sonarrStub.refreshCalls, "RefreshSeries should be called once with the series ID")
}

func TestShowWorkflow_NonVideoFileIsDeletedByProbeAndDownstreamStepsSkipped(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	// Write a plain text file that ffprobe will not recognise as a media file.
	inputPath := filepath.Join(t.TempDir(), "not-a-video.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("not a video"), 0o600))
	outputDir := t.TempDir()

	sonarrStub := &stubEpisodeLibrary{episode: medialib.Episode{ID: 1, SeriesID: 1}}
	wf := NewShowWorkflow(client, ShowWorkflowConfig{OutputDir: outputDir}, sonarrStub, &webhook.Client{})
	startShowWorker(t, client, wf)

	ctx := t.Context()
	// The probe step marks IsValidMedia=false without error; all downstream steps are skipped.
	_, err = wf.Run(ctx, ShowInput{FilePath: inputPath})
	require.NoError(t, err)

	// The probe step deleted the unrecognised file.
	_, statErr := os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "non-video file should be deleted by probe step")

	// Nothing should have been written to the output directory.
	entries, _ := os.ReadDir(outputDir)
	assert.Empty(t, entries, "output directory should be empty when file is not a valid video")

	// Sonarr should not have been called.
	assert.Empty(t, sonarrStub.refreshCalls, "RefreshSeries should not be called for non-video files")
}

func TestShowWorkflow_LookupFailureCausesWorkflowToFail(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	// Sonarr stub returns ErrNotFound for every lookup.
	sonarrStub := &stubEpisodeLibrary{err: medialib.ErrNotFound}
	wf := NewShowWorkflow(client, ShowWorkflowConfig{OutputDir: outputDir}, sonarrStub, &webhook.Client{})
	startShowWorker(t, client, wf)

	ctx := t.Context()
	_, err = wf.Run(ctx, ShowInput{FilePath: inputPath})
	assert.Error(t, err, "workflow should fail when the episode is not found in Sonarr")

	// Source file must not be deleted when a step fails.
	_, statErr := os.Stat(inputPath)
	assert.NoError(t, statErr, "source file should not be deleted when lookup fails")
}
