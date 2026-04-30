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

func TestFinalize_ValidPath_NotifiesLibraryAndCleansSource(t *testing.T) {
	srcDir := t.TempDir()

	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	radarr := &stubLibraryClient{}
	a, reader := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:  FinalizeValid,
		Input: MediaInput{FilePath: srcPath, MediaType: medialib.MovieType, OutputPath: "/out"},
		Probe: steps.ProbeOutput{IsValidMedia: true, VideoCodec: "h264", Format: "mp4"},
		Transcode: steps.TranscodeOutput{
			DestCodec: "hevc", DestContainer: "mkv", DestFilePath: "/out/movie.mkv",
		},
	})
	require.NoError(t, err)

	require.Len(t, radarr.importCalls, 1)
	assert.Equal(t, "/out/movie.mkv", radarr.importCalls[0])

	_, statErr := os.Stat(srcPath)
	assert.True(t, os.IsNotExist(statErr), "source should be deleted by valid-path finalize")

	rm := collectActivityMetrics(t, reader)
	require.NotNil(t, findMetric(rm, "media_workflow_total_duration_seconds"), "valid-path finalize should record per-run metrics")
}

func TestFinalize_ValidPath_PreserveSourceWritesSentinel(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	radarr := &stubLibraryClient{}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode: FinalizeValid,
		Input: MediaInput{
			FilePath: srcPath, MediaType: medialib.MovieType, OutputPath: "/out", PreserveSource: true,
		},
		Probe:     steps.ProbeOutput{IsValidMedia: true},
		Transcode: steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	})
	require.NoError(t, err)

	_, statErr := os.Stat(srcPath)
	assert.NoError(t, statErr, "source should be preserved when PreserveSource is true")

	sentinel := filepath.Join(srcDir, ".movie.mkv.done")
	_, sentErr := os.Stat(sentinel)
	assert.NoError(t, sentErr, "sentinel should be written next to the preserved source")
}

func TestFinalize_ValidPath_OutputRemotePathSubstitutedInImportCall(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	radarr := &stubLibraryClient{}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode: FinalizeValid,
		Input: MediaInput{
			FilePath: srcPath, MediaType: medialib.MovieType,
			OutputPath: "/processed", OutputRemotePath: "/remote/movies",
		},
		Probe:     steps.ProbeOutput{IsValidMedia: true},
		Transcode: steps.TranscodeOutput{DestFilePath: "/processed/movie.mkv"},
	})
	require.NoError(t, err)

	require.Len(t, radarr.importCalls, 1)
	assert.Equal(t, "/remote/movies/movie.mkv", radarr.importCalls[0])
}

func TestFinalize_InvalidPath_RecordsInvalidFileMetric(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "not-a-video.txt")
	// File deleted by probe before finalize-invalid runs in real flow; the
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

func TestFinalize_FailurePath_SendsWebhookPayload(t *testing.T) {
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

func TestFinalize_FailurePath_NoWebhookUrlIsNoop(t *testing.T) {
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:        FinalizeFailure,
		Input:       MediaInput{FilePath: "/in/movie.mkv"},
		FailureStep: "probe",
		FailureErr:  "boom",
	})
	require.NoError(t, err, "missing webhook URL is acceptable; activity must not error")
}

func TestFinalize_ValidPath_NotifyFailurePropagates(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	radarr := &stubLibraryClient{err: errors.New("radarr unreachable")}
	a, _ := newRecordingActivities(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{})

	err := a.Finalize(t.Context(), FinalizeInput{
		Mode:      FinalizeValid,
		Input:     MediaInput{FilePath: srcPath, MediaType: medialib.MovieType, OutputPath: "/out"},
		Probe:     steps.ProbeOutput{IsValidMedia: true},
		Transcode: steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	})
	require.Error(t, err, "library import failure should propagate")

	_, statErr := os.Stat(srcPath)
	assert.NoError(t, statErr, "source should not be deleted when library import fails")
}
