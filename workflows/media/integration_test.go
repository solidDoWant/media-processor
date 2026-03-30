//go:build integration

package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// startMediaWorker creates a Hatchet worker with the given media workflow and starts it.
// The worker is automatically stopped via t.Cleanup. The call blocks until the worker
// appears as ACTIVE in the Hatchet server (with a 30s timeout) so tests never race
// against a fixed sleep.
func startMediaWorker(t *testing.T, client *hatchet.Client, wf *hatchet.Workflow) {
	t.Helper()
	worker, err := client.NewWorker("test-media-worker", hatchet.WithWorkflows(wf))
	require.NoError(t, err, "create Hatchet worker")

	cleanup, err := worker.Start()
	require.NoError(t, err, "start worker")
	t.Cleanup(func() { _ = cleanup() })

	// Poll until the worker is registered and ACTIVE on the Hatchet server.
	require.Eventually(t, func() bool {
		list, listErr := client.Workers().List(t.Context())
		if listErr != nil || list == nil || list.Rows == nil {
			return false
		}
		for _, w := range *list.Rows {
			if w.Name == "test-media-worker" && w.Status != nil && *w.Status == rest.ACTIVE {
				return true
			}
		}
		return false
	}, 30*time.Second, 250*time.Millisecond, "media worker failed to register within 30s")
}

func TestMediaWorkflow_Movie_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	wf := NewMediaWorkflow(client, MediaWorkflowConfig{OutputDir: outputDir}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, client, wf)

	_, err = wf.Run(t.Context(), MediaInput{FilePath: inputPath, MediaType: medialib.MovieType})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted by cleanup step")
}

func TestMediaWorkflow_Movie_ImportByFilePathIsCalledAfterTranscode(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	radarrStub := &stubLibraryClient{}
	wf := NewMediaWorkflow(client, MediaWorkflowConfig{OutputDir: outputDir}, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, client, wf)

	_, err = wf.Run(t.Context(), MediaInput{FilePath: inputPath, MediaType: medialib.MovieType})
	require.NoError(t, err)

	require.Len(t, radarrStub.importCalls, 1, "ImportByFilePath should be called exactly once")
	assert.True(t, strings.HasPrefix(radarrStub.importCalls[0], outputDir), "ImportByFilePath should be called with the output path, got %q", radarrStub.importCalls[0])
}

func TestMediaWorkflow_Show_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	wf := NewMediaWorkflow(client, MediaWorkflowConfig{OutputDir: outputDir}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, client, wf)

	_, err = wf.Run(t.Context(), MediaInput{FilePath: inputPath, MediaType: medialib.ShowType})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted by cleanup step")
}

func TestMediaWorkflow_Show_ImportByFilePathIsCalledAfterTranscode(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	sonarrStub := &stubLibraryClient{}
	wf := NewMediaWorkflow(client, MediaWorkflowConfig{OutputDir: outputDir}, &stubLibraryClient{}, sonarrStub, &webhook.Client{})
	startMediaWorker(t, client, wf)

	_, err = wf.Run(t.Context(), MediaInput{FilePath: inputPath, MediaType: medialib.ShowType})
	require.NoError(t, err)

	require.Len(t, sonarrStub.importCalls, 1, "ImportByFilePath should be called exactly once")
	assert.True(t, strings.HasPrefix(sonarrStub.importCalls[0], outputDir), "ImportByFilePath should be called with the output path, got %q", sonarrStub.importCalls[0])
}

func TestMediaWorkflow_NonVideoFileIsDeletedByProbeAndDownstreamStepsSkipped(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := filepath.Join(t.TempDir(), "not-a-video.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("not a video"), 0o600))
	outputDir := t.TempDir()

	wf := NewMediaWorkflow(client, MediaWorkflowConfig{OutputDir: outputDir}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, client, wf)

	_, err = wf.Run(t.Context(), MediaInput{FilePath: inputPath, MediaType: medialib.MovieType})
	require.NoError(t, err)

	_, statErr := os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "non-video file should be deleted by probe step")

	entries, readErr := os.ReadDir(outputDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "output directory should be empty when file is not a valid video")
}

func TestMediaWorkflow_RefreshFailureCausesWorkflowToFail(t *testing.T) {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		t.Skip("HATCHET_CLIENT_TOKEN not set; run 'make hatchet-up' and 'source .env.hatchet' first")
	}

	client, err := hatchet.NewClient()
	require.NoError(t, err)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	radarrStub := &stubLibraryClient{err: medialib.ErrNotFound}
	wf := NewMediaWorkflow(client, MediaWorkflowConfig{OutputDir: outputDir}, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, client, wf)

	_, err = wf.Run(t.Context(), MediaInput{FilePath: inputPath, MediaType: medialib.MovieType})
	assert.Error(t, err, "workflow should fail when the movie is not found in Radarr")

	_, statErr := os.Stat(inputPath)
	assert.NoError(t, statErr, "source file should not be deleted when notify fails")
}
