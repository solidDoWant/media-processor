//go:build integration

package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// newTestClient creates a Temporal client for integration tests, skipping when
// TEMPORAL_ADDRESS is not set.
func newTestClient(t *testing.T) client.Client {
	t.Helper()

	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run 'make temporal-up' first")
	}

	namespace := os.Getenv("TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	c, err := client.NewLazyClient(client.Options{
		HostPort:  addr,
		Namespace: namespace,
	})
	require.NoError(t, err, "create Temporal client")
	t.Cleanup(c.Close)

	return c
}

const testTaskQueue = "test-media-worker"

// startMediaWorker creates and starts a Temporal worker with the media workflow and
// activities registered against testTaskQueue. The worker is stopped via t.Cleanup.
func startMediaWorker(t *testing.T, c client.Client, mw *MediaWorkflows, ma *MediaActivities) {
	t.Helper()

	w := temporalworker.New(c, testTaskQueue, temporalworker.Options{})
	w.RegisterWorkflowWithOptions(mw.MediaWorkflow, workflow.RegisterOptions{Name: MediaWorkflowName})
	w.RegisterActivity(ma)

	require.NoError(t, w.Start(), "start media worker")
	t.Cleanup(w.Stop)
}

// runMediaWorkflow executes the media workflow synchronously and returns any error.
func runMediaWorkflow(t *testing.T, c client.Client, input MediaInput) error {
	t.Helper()

	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{
		TaskQueue: testTaskQueue,
	}, MediaWorkflowName, input)
	require.NoError(t, err, "execute workflow")

	return run.Get(t.Context(), nil)
}

func TestMediaWorkflow_Movie_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	c := newTestClient(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted by cleanup step")
}

func TestMediaWorkflow_Movie_SourcePreservedWhenPreserveSourceIsTrue(t *testing.T) {
	c := newTestClient(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, PreserveSource: true, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.NoError(t, statErr, "source file should be preserved when PreserveSource is true")
}

func TestMediaWorkflow_Movie_ImportByFilePathIsCalledAfterTranscode(t *testing.T) {
	c := newTestClient(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	radarrStub := &stubLibraryClient{}
	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	expectedImportPath := filepath.Join(outputDir, mkvBase)
	require.Len(t, radarrStub.importCalls, 1, "ImportByFilePath should be called exactly once")
	assert.Equal(t, expectedImportPath, radarrStub.importCalls[0], "ImportByFilePath should be called with the output file path")
}

func TestMediaWorkflow_Show_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	c := newTestClient(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{FilePath: inputPath, MediaType: medialib.ShowType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted by cleanup step")
}

func TestMediaWorkflow_Show_ImportByFilePathIsCalledAfterTranscode(t *testing.T) {
	c := newTestClient(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	sonarrStub := &stubLibraryClient{}
	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, &stubLibraryClient{}, sonarrStub, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{FilePath: inputPath, MediaType: medialib.ShowType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	expectedImportPath := filepath.Join(outputDir, mkvBase)
	require.Len(t, sonarrStub.importCalls, 1, "ImportByFilePath should be called exactly once")
	assert.Equal(t, expectedImportPath, sonarrStub.importCalls[0], "ImportByFilePath should be called with the output file path")
}

func TestMediaWorkflow_NonVideoFileIsDeletedByProbeAndDownstreamStepsSkipped(t *testing.T) {
	c := newTestClient(t)

	inputPath := filepath.Join(t.TempDir(), "not-a-video.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("not a video"), 0o600))
	outputDir := t.TempDir()

	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	require.NoError(t, err)

	_, statErr := os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "non-video file should be deleted by probe step")

	entries, readErr := os.ReadDir(outputDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "output directory should be empty when file is not a valid video")
}

func TestMediaWorkflow_Movie_OutputRemotePathSubstitutedInImportCall(t *testing.T) {
	c := newTestClient(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()
	remoteDir := "/remote/movies"

	radarrStub := &stubLibraryClient{}
	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{
		FilePath:         inputPath,
		MediaType:        medialib.MovieType,
		OutputPath:       outputDir,
		OutputRemotePath: remoteDir,
	})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	expectedImportPath := filepath.Join(remoteDir, mkvBase)
	require.Len(t, radarrStub.importCalls, 1, "ImportByFilePath should be called exactly once")
	assert.Equal(t, expectedImportPath, radarrStub.importCalls[0], "ImportByFilePath should receive the remote path")
}

func TestMediaWorkflow_RefreshFailureCausesWorkflowToFail(t *testing.T) {
	c := newTestClient(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	radarrStub := &stubLibraryClient{err: medialib.ErrNotFound}
	mw := NewMediaWorkflows(MediaWorkflowConfig{})
	ma := NewMediaActivities(MediaWorkflowConfig{}, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, mw, ma)

	err := runMediaWorkflow(t, c, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	assert.Error(t, err, "workflow should fail when the movie is not found in Radarr")

	_, statErr := os.Stat(inputPath)
	assert.NoError(t, statErr, "source file should not be deleted when notify fails")
}
