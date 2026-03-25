//go:build integration

package movie

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

// startMovieWorker creates a Hatchet worker with the given MovieWorkflow and starts it.
// The worker is automatically stopped via t.Cleanup. The call blocks briefly to allow
// the worker to register its workflows with Hatchet before dispatching runs.
func startMovieWorker(t *testing.T, client *hatchet.Client, wf *hatchet.Workflow) {
	t.Helper()
	worker, err := client.NewWorker("test-movie-worker", hatchet.WithWorkflows(wf))
	require.NoError(t, err, "create Hatchet worker")

	cleanup, err := worker.Start()
	require.NoError(t, err, "start worker")
	t.Cleanup(func() { _ = cleanup() })

	// Allow time for the worker to register its workflow definitions with the Hatchet server.
	time.Sleep(3 * time.Second)
}

func TestMovieWorkflow_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	radarrStub := &stubMovieLibrary{movie: medialib.Movie{ID: 42, Title: "Test Movie"}}
	wf := NewMovieWorkflow(client, MovieWorkflowConfig{OutputDir: outputDir}, radarrStub, &webhook.Client{})
	startMovieWorker(t, client, wf)

	ctx := t.Context()
	_, err = wf.Run(ctx, MovieInput{FilePath: inputPath})
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

func TestMovieWorkflow_NonVideoFileIsDeletedByProbeAndDownstreamStepsSkipped(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	// Write a plain text file that ffprobe will not recognise as a media file.
	inputPath := filepath.Join(t.TempDir(), "not-a-video.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("not a video"), 0o600))
	outputDir := t.TempDir()

	radarrStub := &stubMovieLibrary{movie: medialib.Movie{ID: 1}}
	wf := NewMovieWorkflow(client, MovieWorkflowConfig{OutputDir: outputDir}, radarrStub, &webhook.Client{})
	startMovieWorker(t, client, wf)

	ctx := t.Context()
	// The probe step marks IsValidMedia=false without error; all downstream steps are skipped.
	_, err = wf.Run(ctx, MovieInput{FilePath: inputPath})
	require.NoError(t, err)

	// The probe step deleted the unrecognised file.
	_, statErr := os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "non-video file should be deleted by probe step")

	// Nothing should have been written to the output directory.
	entries, _ := os.ReadDir(outputDir)
	assert.Empty(t, entries, "output directory should be empty when file is not a valid video")
}

func TestMovieWorkflow_LookupFailureCausesWorkflowToFail(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	// Radarr stub returns ErrNotFound for every lookup.
	radarrStub := &stubMovieLibrary{err: medialib.ErrNotFound}
	wf := NewMovieWorkflow(client, MovieWorkflowConfig{OutputDir: outputDir}, radarrStub, &webhook.Client{})
	startMovieWorker(t, client, wf)

	ctx := t.Context()
	_, err = wf.Run(ctx, MovieInput{FilePath: inputPath})
	assert.Error(t, err, "workflow should fail when the movie is not found in Radarr")
}
