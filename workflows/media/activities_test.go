package media

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// newRecordingActivities builds an Activities backed by a fresh prometheus.Registry
// + the supplied stubs, ready for direct method-level testing.
func newRecordingActivities(t *testing.T, cfg MediaWorkflowConfig, radarr, sonarr medialib.ArrLibrary, wh *webhook.Client) (*Activities, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	cfg.MetricsRegisterer = reg

	a, err := NewActivities(cfg, radarr, sonarr, wh)
	require.NoError(t, err)

	return a, reg
}

func TestNotify_CallsLibraryImport(t *testing.T) {
	radarr := &stubLibraryClient{}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Notify(t.Context(),
		MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType, OutputPath: "/out"},
		steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	)
	require.NoError(t, err)

	require.Len(t, radarr.importCalls, 1)
	assert.Equal(t, "/out/movie.mkv", radarr.importCalls[0])
}

func TestNotify_OutputRemotePathSubstitutedInImportCall(t *testing.T) {
	radarr := &stubLibraryClient{}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Notify(t.Context(),
		MediaInput{
			FilePath: "/in/movie.mkv", MediaType: medialib.MovieType,
			OutputPath: "/processed", OutputRemotePath: "/remote/movies",
		},
		steps.TranscodeOutput{DestFilePath: "/processed/movie.mkv"},
	)
	require.NoError(t, err)

	require.Len(t, radarr.importCalls, 1)
	assert.Equal(t, "/remote/movies/movie.mkv", radarr.importCalls[0])
}

func TestNotify_LibraryImportFailurePropagates(t *testing.T) {
	radarr := &stubLibraryClient{err: errors.New("radarr unreachable")}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Notify(t.Context(),
		MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType, OutputPath: "/out"},
		steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	)
	require.Error(t, err, "library import failure should propagate")
}

func TestCleanup_DeletesSource(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Cleanup(t.Context(), MediaInput{FilePath: srcPath, MediaType: medialib.MovieType})
	require.NoError(t, err)

	_, statErr := os.Stat(srcPath)
	assert.True(t, os.IsNotExist(statErr), "source should be deleted")
}

func TestCleanup_PreserveSourceWritesSentinel(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Cleanup(t.Context(), MediaInput{
		FilePath: srcPath, MediaType: medialib.MovieType, PreserveSource: true,
	})
	require.NoError(t, err)

	_, statErr := os.Stat(srcPath)
	assert.NoError(t, statErr, "source should be preserved when PreserveSource is true")

	sentinel := filepath.Join(srcDir, ".movie.mkv.done")
	_, sentErr := os.Stat(sentinel)
	assert.NoError(t, sentErr, "sentinel should be written next to the preserved source")
}

func TestCleanup_AlreadyDeletedSourceIsNotAnError(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	// Do NOT create the file: simulate a retried cleanup after the previous
	// attempt already removed it.

	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Cleanup(t.Context(), MediaInput{FilePath: srcPath, MediaType: medialib.MovieType})
	require.NoError(t, err, "cleanup must be idempotent so retries do not fail when the file is already gone")
}

func TestRecordRunMetrics_RecordsRunHistograms(t *testing.T) {
	a, reg := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.RecordRunMetrics(t.Context(),
		MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType},
		steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4"},
		steps.TranscodeOutput{
			DestCodec: "hevc", DestContainer: "mkv", DestFilePath: "/out/movie.mkv",
		},
	)
	require.NoError(t, err)

	require.NotNil(t, findMetricFamily(t, reg, "media_workflow_total_duration_seconds"),
		"metrics activity should record per-run histograms")
}

func TestRecordInvalid_RecordsInvalidFileMetric(t *testing.T) {
	a, reg := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.RecordInvalid(t.Context(), MediaInput{
		FilePath: "/in/not-a-video.txt", MediaType: medialib.MovieType, MappingName: "downloads",
	})
	require.NoError(t, err)

	mf := findMetricFamily(t, reg, "media_workflow_invalid_files_total")
	require.NotNil(t, mf, "invalid_files_total counter should be present")
	require.Len(t, mf.GetMetric(), 1)
	assert.EqualValues(t, 1, mf.GetMetric()[0].GetCounter().GetValue())
}

func TestNotifyFailure_SendsWebhookPayload(t *testing.T) {
	var (
		called    bool
		payload   map[string]string
		bodyError error
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		bodyError = json.NewDecoder(r.Body).Decode(&payload)

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{URL: srv.URL})

	err := a.NotifyFailure(t.Context(), MediaInput{FilePath: "/in/movie.mkv"}, "transcode", "ffmpeg exited with code 1")
	require.NoError(t, err)
	require.NoError(t, bodyError)
	require.True(t, called, "webhook should be invoked")

	assert.Equal(t, MediaWorkflowName, payload["workflow"])
	assert.Equal(t, "/in/movie.mkv", payload["file_path"])
	assert.Equal(t, "transcode", payload["step"])
	assert.Equal(t, "transcode: ffmpeg exited with code 1", payload["error"])
}

func TestNotifyFailure_NoWebhookUrlIsNoop(t *testing.T) {
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.NotifyFailure(t.Context(), MediaInput{FilePath: "/in/movie.mkv"}, "probe", "boom")
	require.NoError(t, err, "missing webhook URL is acceptable; activity must not error")
}
