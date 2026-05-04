//go:build integration

package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// startMediaWorker registers the media workflow and every activity across
// per-token Temporal Workers, mirroring the production layout. The workflow
// runs on taskQueue; each activity runs on "{taskQueue}-{activity-token}". All
// Workers are stopped via t.Cleanup.
func startMediaWorker(t *testing.T, c client.Client, taskQueue string, activities *Activities) {
	t.Helper()

	tokens := append([]string{WorkflowToken}, KnownActivities...)
	startMediaWorkerForTokens(t, c, taskQueue, activities, tokens)
}

// startMediaWorkerForTokens starts one Worker per token in tokens, attaching
// only the matching workflow function or activity. Used by the split-worker
// test to put different tokens on different worker pools.
func startMediaWorkerForTokens(t *testing.T, c client.Client, taskQueue string, activities *Activities, tokens []string) {
	t.Helper()

	for _, token := range tokens {
		var (
			queue string
			w     worker.Worker
		)

		if token == WorkflowToken {
			queue = taskQueue
			w = worker.New(c, queue, worker.Options{})
			activities.RegisterWorkflow(w)
		} else {
			queue = ActivityTaskQueue(taskQueue, token)
			w = worker.New(c, queue, worker.Options{})
			require.NoError(t, activities.RegisterActivity(w, token), "register %s", token)
		}

		require.NoError(t, w.Start(), "start worker for %s on %s", token, queue)
		t.Cleanup(w.Stop)
	}
}

// runMediaWorkflow starts a workflow execution on the given task queue and
// blocks until it completes. The workflow ID is unique per call.
func runMediaWorkflow(t *testing.T, c client.Client, taskQueue string, input MediaInput) error {
	t.Helper()

	options := client.StartWorkflowOptions{
		ID:        "media-test-" + filepath.Base(input.FilePath) + "-" + time.Now().Format("150405.000000000"),
		TaskQueue: taskQueue,
	}

	wf, err := c.ExecuteWorkflow(t.Context(), options, MediaWorkflowName, input)
	require.NoError(t, err, "ExecuteWorkflow")

	return wf.Get(t.Context(), nil)
}

// dialTemporal builds a Temporal client. Returns the client + the task queue to
// use for tests; both are unique per test invocation so parallel tests do not
// pick up each other's tasks.
func dialTemporal(t *testing.T) (client.Client, string) {
	t.Helper()

	c, err := client.Dial(client.Options{
		HostPort:  os.Getenv("TEMPORAL_ADDRESS"),
		Namespace: os.Getenv("TEMPORAL_NAMESPACE"),
	})
	require.NoError(t, err, "dial Temporal")

	t.Cleanup(c.Close)

	taskQueue := "media-test-" + t.Name() + "-" + time.Now().Format("150405.000000000")

	return c, taskQueue
}

func newTestActivities(t *testing.T, taskQueue string, radarr, sonarr medialib.ArrLibrary, wh *webhook.Client) *Activities {
	t.Helper()

	a, err := NewActivities(MediaWorkflowConfig{TaskQueuePrefix: taskQueue}, radarr, sonarr, wh)
	require.NoError(t, err)

	return a
}

func TestMediaWorkflow_Movie_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	a := newTestActivities(t, taskQueue, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted by Cleanup activity")
}

func TestMediaWorkflow_Movie_SourcePreservedWhenPreserveSourceIsTrue(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	a := newTestActivities(t, taskQueue, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, PreserveSource: true, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.NoError(t, statErr, "source file should be preserved when PreserveSource is true")
}

func TestMediaWorkflow_Movie_ImportByFilePathIsCalledAfterTranscode(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	radarrStub := &stubLibraryClient{}
	a := newTestActivities(t, taskQueue, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	expectedImportPath := filepath.Join(outputDir, mkvBase)

	require.Len(t, radarrStub.importCalls, 1, "ImportByFilePath should be called exactly once")
	assert.Equal(t, expectedImportPath, radarrStub.importCalls[0], "ImportByFilePath should be called with the output file path")
}

func TestMediaWorkflow_Show_ValidVideoIsTranscodedAndSourceDeleted(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	a := newTestActivities(t, taskQueue, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.ShowType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist in outputDir")

	_, statErr = os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted by Cleanup activity")
}

func TestMediaWorkflow_Show_ImportByFilePathIsCalledAfterTranscode(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	sonarrStub := &stubLibraryClient{}
	a := newTestActivities(t, taskQueue, &stubLibraryClient{}, sonarrStub, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.ShowType, OutputPath: outputDir})
	require.NoError(t, err)

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	expectedImportPath := filepath.Join(outputDir, mkvBase)

	require.Len(t, sonarrStub.importCalls, 1, "ImportByFilePath should be called exactly once")
	assert.Equal(t, expectedImportPath, sonarrStub.importCalls[0], "ImportByFilePath should be called with the output file path")
}

func TestMediaWorkflow_NonVideoFileIsDeletedByProbeAndDownstreamStepsSkipped(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := filepath.Join(t.TempDir(), "not-a-video.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("not a video"), 0o600))
	outputDir := t.TempDir()

	a := newTestActivities(t, taskQueue, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	require.NoError(t, err)

	_, statErr := os.Stat(inputPath)
	assert.True(t, os.IsNotExist(statErr), "non-video file should be deleted by probe step")

	entries, readErr := os.ReadDir(outputDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "output directory should be empty when file is not a valid video")
}

func TestMediaWorkflow_Movie_OutputRemotePathSubstitutedInImportCall(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()
	remoteDir := "/remote/movies"

	radarrStub := &stubLibraryClient{}
	a := newTestActivities(t, taskQueue, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{
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

// TestMediaWorkflow_SplitWorkers_TranscodeOnDedicatedPool runs the workflow
// with two disjoint worker pools: one polling only the transcode activity
// queue, one polling the workflow queue and every other activity queue. The
// pools register distinct *Activities instances backed by separate stubs, so
// after the run we can assert which pool's Activities serviced which
// activity.
func TestMediaWorkflow_SplitWorkers_TranscodeOnDedicatedPool(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	transcodePoolRadarr := &stubLibraryClient{}
	restPoolRadarr := &stubLibraryClient{}

	transcodePoolActivities := newTestActivities(t, taskQueue, transcodePoolRadarr, &stubLibraryClient{}, &webhook.Client{})
	restPoolActivities := newTestActivities(t, taskQueue, restPoolRadarr, &stubLibraryClient{}, &webhook.Client{})

	startMediaWorkerForTokens(t, c, taskQueue, transcodePoolActivities, []string{TranscodeActivityToken})
	startMediaWorkerForTokens(t, c, taskQueue, restPoolActivities, []string{
		WorkflowToken,
		ProbeActivityToken,
		DetectCropActivityToken,
		NotifyActivityToken,
		CleanupActivityToken,
		NotifyFailureActivityToken,
	})

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	require.NoError(t, err, "split-worker workflow should complete end-to-end")

	inputBase := filepath.Base(inputPath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	_, statErr := os.Stat(filepath.Join(outputDir, mkvBase))
	assert.NoError(t, statErr, "transcoded output file should exist")

	assert.Empty(t, transcodePoolRadarr.importCalls, "Notify (which calls ImportByFilePath) must run on the rest pool, not the transcode pool")
	require.Len(t, restPoolRadarr.importCalls, 1, "Notify must run exactly once on the rest pool")
}

func TestMediaWorkflow_RefreshFailureCausesWorkflowToFail(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	c, taskQueue := dialTemporal(t)

	inputPath := copyTestVideo(t)
	outputDir := t.TempDir()

	radarrStub := &stubLibraryClient{err: medialib.ErrNotFound}
	a := newTestActivities(t, taskQueue, radarrStub, &stubLibraryClient{}, &webhook.Client{})
	startMediaWorker(t, c, taskQueue, a)

	err := runMediaWorkflow(t, c, taskQueue, MediaInput{FilePath: inputPath, MediaType: medialib.MovieType, OutputPath: outputDir})
	assert.Error(t, err, "workflow should fail when the movie is not found in Radarr")

	_, statErr := os.Stat(inputPath)
	assert.NoError(t, statErr, "source file should not be deleted when notify fails")
}
