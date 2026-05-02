package media

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally/v4"
	contribtally "go.temporal.io/sdk/contrib/tally"
	"go.temporal.io/sdk/testsuite"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// newActivityEnv builds an Activities backed by the supplied stubs and a
// TestActivityEnvironment with all activities registered. The supplied tally
// TestScope is wired through as the SDK metrics handler so emissions made via
// activity.GetMetricsHandler land in the snapshot.
func newActivityEnv(t *testing.T, cfg MediaWorkflowConfig, radarr, sonarr medialib.ArrLibrary, wh *webhook.Client, scope tally.TestScope) (*Activities, *testsuite.TestActivityEnvironment) {
	t.Helper()

	a, err := NewActivities(cfg, radarr, sonarr, wh)
	require.NoError(t, err)

	suite := &testsuite.WorkflowTestSuite{}
	if scope != nil {
		suite.SetMetricsHandler(contribtally.NewMetricsHandler(scope))
	}

	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(a.Probe)
	env.RegisterActivity(a.DetectCrop)
	env.RegisterActivity(a.Transcode)
	env.RegisterActivity(a.Notify)
	env.RegisterActivity(a.Cleanup)
	env.RegisterActivity(a.NotifyFailure)

	return a, env
}

func TestNotify_CallsLibraryImport(t *testing.T) {
	radarr := &stubLibraryClient{}
	a, env := newActivityEnv(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{}, nil)

	_, err := env.ExecuteActivity(a.Notify,
		MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType, OutputPath: "/out"},
		steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	)
	require.NoError(t, err)

	require.Len(t, radarr.importCalls, 1)
	assert.Equal(t, "/out/movie.mkv", radarr.importCalls[0])
}

func TestNotify_OutputRemotePathSubstitutedInImportCall(t *testing.T) {
	radarr := &stubLibraryClient{}
	a, env := newActivityEnv(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{}, nil)

	_, err := env.ExecuteActivity(a.Notify,
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
	a, env := newActivityEnv(t, MediaWorkflowConfig{}, radarr, &stubLibraryClient{}, &webhook.Client{}, nil)

	_, err := env.ExecuteActivity(a.Notify,
		MediaInput{FilePath: "/in/movie.mkv", MediaType: medialib.MovieType, OutputPath: "/out"},
		steps.TranscodeOutput{DestFilePath: "/out/movie.mkv"},
	)
	require.Error(t, err, "library import failure should propagate")
}

func TestCleanup_DeletesSource(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	a, env := newActivityEnv(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{}, nil)

	_, err := env.ExecuteActivity(a.Cleanup, MediaInput{FilePath: srcPath, MediaType: medialib.MovieType})
	require.NoError(t, err)

	_, statErr := os.Stat(srcPath)
	assert.True(t, os.IsNotExist(statErr), "source should be deleted")
}

func TestCleanup_PreserveSourceWritesSentinel(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "movie.mkv")
	require.NoError(t, os.WriteFile(srcPath, []byte("source"), 0o600))

	a, env := newActivityEnv(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{}, nil)

	_, err := env.ExecuteActivity(a.Cleanup, MediaInput{
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

	a, env := newActivityEnv(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{}, nil)

	_, err := env.ExecuteActivity(a.Cleanup, MediaInput{FilePath: srcPath, MediaType: medialib.MovieType})
	require.NoError(t, err, "cleanup must be idempotent so retries do not fail when the file is already gone")
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

	a, env := newActivityEnv(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{URL: srv.URL}, nil)

	_, err := env.ExecuteActivity(a.NotifyFailure, MediaInput{FilePath: "/in/movie.mkv"}, "transcode", "ffmpeg exited with code 1")
	require.NoError(t, err)
	require.NoError(t, bodyError)
	require.True(t, called, "webhook should be invoked")

	assert.Equal(t, MediaWorkflowName, payload["workflow"])
	assert.Equal(t, "/in/movie.mkv", payload["file_path"])
	assert.Equal(t, "transcode", payload["step"])
	assert.Equal(t, "transcode: ffmpeg exited with code 1", payload["error"])
}

func TestNotifyFailure_NoWebhookUrlIsNoop(t *testing.T) {
	a, env := newActivityEnv(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{}, nil)

	_, err := env.ExecuteActivity(a.NotifyFailure, MediaInput{FilePath: "/in/movie.mkv"}, "probe", "boom")
	require.NoError(t, err, "missing webhook URL is acceptable; activity must not error")
}

// TestResolveHighCardinalityLabels_StableKeySetAcrossOutcomes guards against
// the tally→Prometheus registration-conflict pitfall: every outcome of the
// arr-library lookup (success, error, nil-info) must produce the same set
// of tag keys, otherwise the tally cached reporter would attempt two
// Prometheus registrations of the same metric name with different label
// names — the second one becoming a noopMetric and silently dropping
// observations for whichever outcome registered second.
func TestResolveHighCardinalityLabels_StableKeySetAcrossOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		stub   *stubLibraryClient
		input  MediaInput
		assert func(t *testing.T, tags map[string]string)
	}{
		{
			name:  "success movie populates id/title/year and leaves episode keys empty",
			stub:  &stubLibraryClient{infoResult: &medialib.Movie{ID: 1, Title: "Movie", Year: 2020}},
			input: MediaInput{MediaType: medialib.MovieType, FilePath: "/x"},
			assert: func(t *testing.T, tags map[string]string) {
				assert.Equal(t, "1", tags["id"])
				assert.Equal(t, "", tags["series_title"])
			},
		},
		{
			name:   "GetInfo error returns empty values for every key",
			stub:   &stubLibraryClient{infoErr: errors.New("radarr down")},
			input:  MediaInput{MediaType: medialib.MovieType, FilePath: "/x"},
			assert: func(t *testing.T, tags map[string]string) { assert.Equal(t, "", tags["id"]) },
		},
		{
			name:   "GetInfo nil result returns empty values for every key",
			stub:   &stubLibraryClient{},
			input:  MediaInput{MediaType: medialib.MovieType, FilePath: "/x"},
			assert: func(t *testing.T, tags map[string]string) { assert.Equal(t, "", tags["id"]) },
		},
	}

	wantKeys := []string{"episode_number", "id", "season_number", "series_title", "title", "year"}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewActivities(MediaWorkflowConfig{HighCardinalityLabels: true}, tc.stub, &stubLibraryClient{}, &webhook.Client{})
			require.NoError(t, err)

			// resolveHighCardinalityLabels is a method, not a registered
			// activity. Wrap it in an inline activity so its inner
			// activity.GetMetricsHandler / GetLogger calls find a real
			// activity context.
			wrap := func(ctx context.Context, in MediaInput) (map[string]string, error) {
				return a.resolveHighCardinalityLabels(ctx, in, tc.stub), nil
			}

			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(wrap)

			val, err := env.ExecuteActivity(wrap, tc.input)
			require.NoError(t, err)

			var got map[string]string
			require.NoError(t, val.Get(&got))

			assert.Equal(t, wantKeys, slices.Sorted(maps.Keys(got)),
				"key set must be identical across all GetInfo outcomes")
			tc.assert(t, got)
		})
	}
}

// TestProbe_InvalidMediaEmitsCounterOnly probes a non-media input and verifies
// only the invalid-files counter is incremented; the per-run histograms and
// gauges are skipped because there is no media to describe.
func TestProbe_InvalidMediaEmitsCounterOnly(t *testing.T) {
	scope := tally.NewTestScope("", nil)
	a, env := newActivityEnv(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{}, scope)

	notMedia := filepath.Join(t.TempDir(), "not-a-video.txt")
	require.NoError(t, os.WriteFile(notMedia, []byte("hello"), 0o600))

	_, err := env.ExecuteActivity(a.Probe, MediaInput{
		FilePath: notMedia, MediaType: medialib.MovieType, MappingName: "downloads",
	})
	require.NoError(t, err)

	snap := scope.Snapshot()
	wantTags := map[string]string{"media_type": "movie", "mapping_name": "downloads"}

	counter := findCounter(t, snap, "media_workflow_invalid_files", wantTags)
	assert.EqualValues(t, 1, counter.Value())

	assert.Empty(t, findHistograms(snap, "media_workflow_audio_track_count"), "track histograms must not be emitted for invalid files")
	assert.Empty(t, findHistograms(snap, "media_workflow_subtitle_track_count"))
	assert.Empty(t, findHistograms(snap, "media_workflow_source_duration_seconds"))
}

// TestProbe_ValidMediaEmitsHistogramsAndGauges runs probe against a real
// fixture video and verifies the source-duration histogram + audio/subtitle
// gauges are emitted with the base tag set.
func TestProbe_ValidMediaEmitsHistogramsAndGauges(t *testing.T) {
	scope := tally.NewTestScope("", nil)
	a, env := newActivityEnv(t, MediaWorkflowConfig{}, &stubLibraryClient{}, &stubLibraryClient{}, &webhook.Client{}, scope)

	srcPath := copyTestVideo(t)

	_, err := env.ExecuteActivity(a.Probe, MediaInput{
		FilePath: srcPath, MediaType: medialib.MovieType, MappingName: "downloads",
	})
	require.NoError(t, err)

	snap := scope.Snapshot()
	wantTags := map[string]string{"media_type": "movie", "mapping_name": "downloads"}

	durHist := findHistogram(t, snap, "media_workflow_source_duration_seconds", wantTags)
	assertHistogramSampleCount(t, durHist, 1)

	audioGauge := findGauge(t, snap, "media_workflow_audio_track_count", wantTags)
	assert.GreaterOrEqual(t, audioGauge.Value(), float64(0))

	_ = findGauge(t, snap, "media_workflow_subtitle_track_count", wantTags)
}

// hasSubsetTags returns true when actual contains every key/value pair in
// want. Used to look up tally snapshots by metric name + an application
// subset of tags, ignoring SDK-injected worker tags
// (activity_type, namespace, task_queue, workflow_type) that vary by env.
func hasSubsetTags(actual, want map[string]string) bool {
	for k, v := range want {
		if actual[k] != v {
			return false
		}
	}

	return true
}

func findCounter(t *testing.T, snap tally.Snapshot, name string, wantTags map[string]string) tally.CounterSnapshot {
	t.Helper()

	for _, c := range snap.Counters() {
		if c.Name() == name && hasSubsetTags(c.Tags(), wantTags) {
			return c
		}
	}

	t.Fatalf("counter %q with tags %v not in snapshot; got %v", name, wantTags, allCounterKeys(snap))

	return nil
}

func findGauge(t *testing.T, snap tally.Snapshot, name string, wantTags map[string]string) tally.GaugeSnapshot {
	t.Helper()

	for _, g := range snap.Gauges() {
		if g.Name() == name && hasSubsetTags(g.Tags(), wantTags) {
			return g
		}
	}

	t.Fatalf("gauge %q with tags %v not in snapshot; got %v", name, wantTags, allGaugeKeys(snap))

	return nil
}

func findHistogram(t *testing.T, snap tally.Snapshot, name string, wantTags map[string]string) tally.HistogramSnapshot {
	t.Helper()

	for _, h := range snap.Histograms() {
		if h.Name() == name && hasSubsetTags(h.Tags(), wantTags) {
			return h
		}
	}

	t.Fatalf("histogram %q with tags %v not in snapshot; got %v", name, wantTags, allHistogramKeys(snap))

	return nil
}

// findHistograms returns every snapshot whose metric name matches.
func findHistograms(snap tally.Snapshot, name string) []tally.HistogramSnapshot {
	var out []tally.HistogramSnapshot

	for _, h := range snap.Histograms() {
		if h.Name() == name {
			out = append(out, h)
		}
	}

	return out
}

// assertHistogramSampleCount verifies that exactly want samples were recorded
// across all of the histogram's buckets (value- or duration-typed).
func assertHistogramSampleCount(t *testing.T, h tally.HistogramSnapshot, want int64) {
	t.Helper()

	var total int64
	for _, n := range h.Values() {
		total += n
	}

	for _, n := range h.Durations() {
		total += n
	}

	assert.EqualValues(t, want, total, "histogram %q should have %d samples", h.Name(), want)
}

func allCounterKeys(snap tally.Snapshot) []string {
	out := make([]string, 0, len(snap.Counters()))
	for k := range snap.Counters() {
		out = append(out, k)
	}

	return out
}

func allGaugeKeys(snap tally.Snapshot) []string {
	out := make([]string, 0, len(snap.Gauges()))
	for k := range snap.Gauges() {
		out = append(out, k)
	}

	return out
}

func allHistogramKeys(snap tally.Snapshot) []string {
	out := make([]string, 0, len(snap.Histograms()))
	for k := range snap.Histograms() {
		out = append(out, k)
	}

	return out
}
