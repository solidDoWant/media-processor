package media

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// newRecordingActivities builds an Activities backed by a manual metric reader
// + the supplied stubs, ready for direct method-level testing.
func newRecordingActivities(t *testing.T, cfg MediaWorkflowConfig, radarr, sonarr medialib.ArrLibrary, wh *webhook.Client) (*Activities, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })

	cfg.MeterProvider = provider

	a, err := NewActivities(cfg, radarr, sonarr, wh)
	require.NoError(t, err)

	return a, reader
}

func collectActivityMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	return rm
}

func TestFinalize_Notify_CallsLibraryImport(t *testing.T) {
	radarr := &stubLibraryClient{}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:      FinalizeNotify,
		Input:     MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType, OutputPath: "/out"},
		Probe:     steps.ProbeOutput{IsValidMedia: true},
		Transcode: steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	})
	require.NoError(t, err)

	require.Len(t, radarr.importCalls, 1)
	assert.Equal(t, "/out/movie.mkv", radarr.importCalls[0])
}

func TestFinalize_Notify_OutputRemotePathSubstitutedInImportCall(t *testing.T) {
	radarr := &stubLibraryClient{}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode: FinalizeNotify,
		Input: MediaInput{
			FilePath: "/in/movie.mkv", MediaType: medialib.MovieType,
			OutputPath: "/processed", OutputRemotePath: "/remote/movies",
		},
		Probe:     steps.ProbeOutput{IsValidMedia: true},
		Transcode: steps.TranscodeOutput{DestFilePath: "/processed/movie.mkv"},
	})
	require.NoError(t, err)

	require.Len(t, radarr.importCalls, 1)
	assert.Equal(t, "/remote/movies/movie.mkv", radarr.importCalls[0])
}

func TestFinalize_Notify_LibraryImportFailurePropagates(t *testing.T) {
	radarr := &stubLibraryClient{err: errors.New("radarr unreachable")}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:      FinalizeNotify,
		Input:     MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType, OutputPath: "/out"},
		Probe:     steps.ProbeOutput{IsValidMedia: true},
		Transcode: steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	})
	require.Error(t, err, "library import failure should propagate")
}

func TestFinalize_Cleanup_DeletesSource(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:  FinalizeCleanup,
		Input: MediaInput{FilePath: srcPath, MediaType: medialib.MovieType},
	})
	require.NoError(t, err)

	_, statErr := os.Stat(srcPath)
	assert.True(t, os.IsNotExist(statErr), "source should be deleted by cleanup mode")
}

func TestFinalize_Cleanup_PreserveSourceWritesSentinel(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:  FinalizeCleanup,
		Input: MediaInput{FilePath: srcPath, MediaType: medialib.MovieType, PreserveSource: true},
	})
	require.NoError(t, err)

	_, statErr := os.Stat(srcPath)
	assert.NoError(t, statErr, "source should be preserved when PreserveSource is true")

	sentinel := filepath.Join(srcDir, ".movie.mkv.done")
	_, sentErr := os.Stat(sentinel)
	assert.NoError(t, sentErr, "sentinel should be written next to the preserved source")
}

func TestFinalize_Cleanup_AlreadyDeletedSourceIsNotAnError(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	// Do NOT create the file: simulate a retried cleanup after the previous
	// attempt already removed it.

	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:  FinalizeCleanup,
		Input: MediaInput{FilePath: srcPath, MediaType: medialib.MovieType},
	})
	require.NoError(t, err, "cleanup must be idempotent so retries do not fail when the file is already gone")
}

func TestFinalize_Metrics_RecordsRunHistograms(t *testing.T) {
	a, reader := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:  FinalizeMetrics,
		Input: MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType},
		Probe: steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4"},
		Transcode: steps.TranscodeOutput{
			DestCodec: "hevc", DestContainer: "mkv", DestFilePath: "/out/movie.mkv",
		},
	})
	require.NoError(t, err)

	rm := collectActivityMetrics(t, reader)
	require.NotNil(t, rm.ScopeMetrics, "metrics mode should record per-run histograms")
	require.NotNil(t, findMetric(rm, "media_workflow_total_duration_seconds"))
}

func TestFinalize_Invalid_RecordsInvalidFileMetric(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "not-a-video.txt")
	// Probe deletes the file before this activity runs in the real flow; the
	// activity must tolerate the missing source.
	a, reader := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode: FinalizeInvalid,
		Input: MediaInput{
			FilePath: srcPath, MediaType: medialib.MovieType, MappingName: "downloads",
		},
		Probe: steps.ProbeOutput{IsValidMedia: false},
	})
	require.NoError(t, err)

	rm := collectActivityMetrics(t, reader)

	m := findMetric(rm, "media_workflow_invalid_files_total")
	require.NotNil(t, m, "invalid_files_total counter should be present")
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 1)
	assert.EqualValues(t, 1, sum.DataPoints[0].Value)
}

func TestFinalize_Failure_SendsWebhookPayload(t *testing.T) {
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

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:        FinalizeFailure,
		Input:       MediaInput{FilePath: "/in/movie.mkv"},
		FailureStep: "transcode",
		FailureErr:  "ffmpeg exited with code 1",
	})
	require.NoError(t, err)
	require.NoError(t, bodyError)
	require.True(t, called, "webhook should be invoked for failure mode")

	assert.Equal(t, MediaWorkflowName, payload["workflow"])
	assert.Equal(t, "/in/movie.mkv", payload["file_path"])
	assert.Equal(t, "transcode", payload["step"])
	assert.Equal(t, "transcode: ffmpeg exited with code 1", payload["error"])
}

func TestFinalize_Failure_NoWebhookUrlIsNoop(t *testing.T) {
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:        FinalizeFailure,
		Input:       MediaInput{FilePath: "/in/movie.mkv"},
		FailureStep: "probe",
		FailureErr:  "boom",
	})
	require.NoError(t, err, "missing webhook URL is acceptable; activity must not error")
}
