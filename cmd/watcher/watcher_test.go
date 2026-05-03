package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/internal/watcherconfig"
	"github.com/solidDoWant/media-processor/pkg/medialib"
	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
)

// noopInstruments returns a scanInstruments registered against a private throwaway registry.
func noopInstruments(t *testing.T) *scanInstruments {
	t.Helper()

	inst, err := newScanInstruments(prometheus.NewRegistry())
	require.NoError(t, err)

	return inst
}

// newTestInstruments returns a scanInstruments backed by a fresh registry so
// metric values can be inspected in acceptance tests.
func newTestInstruments(t *testing.T) (*scanInstruments, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()

	inst, err := newScanInstruments(reg)
	require.NoError(t, err)

	return inst, reg
}

// findMetricFamily returns the *dto.MetricFamily whose name matches, or nil.
func findMetricFamily(t *testing.T, reg prometheus.Gatherer, name string) *dto.MetricFamily {
	t.Helper()

	mfs, err := reg.Gather()
	require.NoError(t, err)

	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}

	return nil
}

// labelValue returns the value of the named label on m, or ("", false) if absent.
func labelValue(m *dto.Metric, name string) (string, bool) {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue(), true
		}
	}

	return "", false
}

// findCounter returns the *dto.Metric in mf whose label set matches attrs, or nil.
// Each entry of attrs must be present and equal on the metric.
func findCounter(mf *dto.MetricFamily, attrs map[string]string) *dto.Metric {
	if mf == nil {
		return nil
	}

	for _, m := range mf.GetMetric() {
		match := true

		for k, v := range attrs {
			actual, ok := labelValue(m, k)
			if !ok || actual != v {
				match = false
				break
			}
		}

		if match {
			return m
		}
	}

	return nil
}

// TestValidateWatchDirs verifies that validateWatchDirs returns a descriptive error
// when a configured watch directory does not exist, and succeeds when all dirs are present.
func TestValidateWatchDirs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *Config
		errFunc require.ErrorAssertionFunc
	}{
		{
			name: "existing directory passes",
			cfg: &Config{
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: t.TempDir(), MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
				},
			},
			errFunc: require.NoError,
		},
		{
			name: "missing directory returns error",
			cfg: &Config{
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: "/nonexistent/path/abc123", MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
				},
			},
			errFunc: require.Error,
		},
		{
			name: "all errors reported when multiple dirs are missing",
			cfg: &Config{
				Watches: []WatchEntry{
					{Name: "alpha", WatchedPath: "/nonexistent/alpha", MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
					{Name: "beta", WatchedPath: "/nonexistent/beta", MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
				},
			},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.Error(t, err, msgAndArgs...)
				assert.Contains(t, err.Error(), "/nonexistent/alpha")
				assert.Contains(t, err.Error(), "/nonexistent/beta")
			},
		},
		{
			name:    "empty watch list passes",
			cfg:     &Config{},
			errFunc: require.NoError,
		},
		{
			name: "path that exists but is a file returns error",
			cfg: func() *Config {
				f, err := os.CreateTemp(t.TempDir(), "notadir")
				require.NoError(t, err)
				require.NoError(t, f.Close())

				return &Config{Watches: []WatchEntry{{Name: "movies", WatchedPath: f.Name(), MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}}}}
			}(),
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.Error(t, err, msgAndArgs...)
				assert.Contains(t, err.Error(), "not a directory")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.errFunc(t, validateWatchDirs(tt.cfg))
		})
	}
}

// TestScan_FileInWatchedDir verifies that a file present in a configured watch directory
// is dispatched with the correct media type and absolute file path.
func TestScan_FileInWatchedDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "movie.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	type call struct {
		filePath    string
		mediaType   medialib.MediaType
		mappingName string
	}

	var calls []call

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		calls = append(calls, call{input.FilePath, input.MediaType, input.MappingName})
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.Len(t, calls, 1)
	assert.Equal(t, filePath, calls[0].filePath)
	assert.Equal(t, medialib.MovieType, calls[0].mediaType)
	assert.Equal(t, "movies", calls[0].mappingName)
}

// TestScan_SubdirectoryFilesUseParentMapping verifies that files within subdirectories
// of a configured watch path are dispatched using the parent watch entry's media type.
func TestScan_SubdirectoryFilesUseParentMapping(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subdir := filepath.Join(dir, "show-title")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	filePath := filepath.Join(subdir, "episode.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "shows", WatchedPath: dir, MediaType: medialib.ShowType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var dispatched []string

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		dispatched = append(dispatched, input.FilePath)
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.Len(t, dispatched, 1)
	assert.Equal(t, filePath, dispatched[0])
}

// TestScan_DispatchErrorsAreAggregated verifies that dispatch errors do not abort the
// scan — all files are still processed — and the aggregate error is returned.
func TestScan_DispatchErrorsAreAggregated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var count int

	dispatch := func(_ context.Context, _ mediatypes.MediaInput) error {
		count++
		return errors.New("simulated dispatch failure")
	}

	require.Error(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	assert.Equal(t, 2, count)
}

// TestScan_ContextCancellationStopsWalk verifies that cancelling the context causes scan
// to stop walking and return the context error rather than an aggregate scan error.
func TestScan_ContextCancellationStopsWalk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately before scan starts

	err := scan(ctx, cfg, noopInstruments(t), func(_ context.Context, _ mediatypes.MediaInput) error {
		return nil
	})
	assert.ErrorIs(t, err, context.Canceled)
}

// TestScan_MultipleWatchEntries verifies that files in separate watch directories are
// each dispatched with their respective configured media types.
func TestScan_MultipleWatchEntries(t *testing.T) {
	t.Parallel()

	movieDir := t.TempDir()
	showDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(movieDir, "movie.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(showDir, "show.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: movieDir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
			{Name: "shows", WatchedPath: showDir, MediaType: medialib.ShowType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	dispatched := make(map[string]medialib.MediaType) // path → media type
	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		dispatched[input.FilePath] = input.MediaType
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))

	assert.Equal(t, medialib.MovieType, dispatched[filepath.Join(movieDir, "movie.mkv")])
	assert.Equal(t, medialib.ShowType, dispatched[filepath.Join(showDir, "show.mkv")])
}

// TestScan_MetricsPresenceAfterScan verifies that watcher_scans_total,
// watcher_scan_duration_seconds, and watcher_last_successful_scan_unix_seconds are
// all present in the metric output after at least one scan has completed.
func TestScan_MetricsPresenceAfterScan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return nil
	}))

	for _, name := range []string{
		"watcher_scans_total",
		"watcher_scan_duration_seconds",
		"watcher_last_successful_scan_unix_seconds",
	} {
		assert.NotNil(t, findMetricFamily(t, reg, name), "expected metric %q to be present", name)
	}
}

// TestScan_SuccessCounterIncrements verifies that watcher_scans_total{status="success",
// mapping_name="..."} increments by 1 when a mapping's walk completes without errors.
func TestScan_SuccessCounterIncrements(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return nil
	}))

	mf := findMetricFamily(t, reg, "watcher_scans_total")
	require.NotNil(t, mf, "watcher_scans_total should be present")

	m := findCounter(mf, map[string]string{"mapping_name": "movies", "status": "success"})
	require.NotNil(t, m, "expected data point with mapping_name=movies status=success")
	assert.EqualValues(t, 1, m.GetCounter().GetValue())
}

// TestScan_ErrorCounterIncrements verifies that watcher_scans_total{status="error",
// mapping_name="..."} increments when a mapping's walk or dispatch produces at least one error.
func TestScan_ErrorCounterIncrements(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	_ = scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return errors.New("simulated dispatch failure")
	})

	mf := findMetricFamily(t, reg, "watcher_scans_total")
	require.NotNil(t, mf)

	m := findCounter(mf, map[string]string{"mapping_name": "movies", "status": "error"})
	require.NotNil(t, m, "expected data point with mapping_name=movies status=error")
	assert.EqualValues(t, 1, m.GetCounter().GetValue())
}

// TestScan_DurationObservedPerMapping verifies that watcher_scan_duration_seconds
// records one observation per mapping, labelled with mapping_name.
func TestScan_DurationObservedPerMapping(t *testing.T) {
	t.Parallel()

	movieDir := t.TempDir()
	showDir := t.TempDir()

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: movieDir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
			{Name: "shows", WatchedPath: showDir, MediaType: medialib.ShowType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return nil
	}))

	mf := findMetricFamily(t, reg, "watcher_scan_duration_seconds")
	require.NotNil(t, mf)

	require.Len(t, mf.GetMetric(), 2, "expected one histogram series per mapping")

	mappingNames := make(map[string]bool)

	for _, m := range mf.GetMetric() {
		val, ok := labelValue(m, "mapping_name")
		require.True(t, ok, "mapping_name label should be present")

		mappingNames[val] = true

		assert.EqualValues(t, 1, m.GetHistogram().GetSampleCount(), "each mapping should have exactly one observation")
	}

	assert.True(t, mappingNames["movies"])
	assert.True(t, mappingNames["shows"])
}

// TestScan_LastSuccessfulScanSetOnSuccess verifies that watcher_last_successful_scan_unix_seconds
// is set to a non-zero Unix timestamp after a successful mapping scan.
func TestScan_LastSuccessfulScanSetOnSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return nil
	}))

	mf := findMetricFamily(t, reg, "watcher_last_successful_scan_unix_seconds")
	require.NotNil(t, mf)

	require.Len(t, mf.GetMetric(), 1)
	assert.Greater(t, mf.GetMetric()[0].GetGauge().GetValue(), float64(0), "last successful scan timestamp should be non-zero")
}

// TestScan_FilesDiscoveredCounter verifies that watcher_files_discovered_total increments
// by the number of files found, with correct mapping_name and media_type labels.
func TestScan_FilesDiscoveredCounter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return nil
	}))

	mf := findMetricFamily(t, reg, "watcher_files_discovered_total")
	require.NotNil(t, mf)

	m := findCounter(mf, map[string]string{
		"mapping_name": "movies",
		"media_type":   string(medialib.MovieType),
	})
	require.NotNil(t, m, "expected data point with mapping_name=movies media_type=movie")
	assert.EqualValues(t, 2, m.GetCounter().GetValue())
}

// TestScan_DispatchesTotalCounter verifies that watcher_dispatches_total increments by 1
// per successful dispatch with correct mapping_name and media_type labels.
func TestScan_DispatchesTotalCounter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return nil
	}))

	mf := findMetricFamily(t, reg, "watcher_dispatches_total")
	require.NotNil(t, mf)

	m := findCounter(mf, map[string]string{
		"mapping_name": "movies",
		"media_type":   string(medialib.MovieType),
	})
	require.NotNil(t, m, "expected data point with mapping_name=movies media_type=movie")
	assert.EqualValues(t, 1, m.GetCounter().GetValue())
}

// TestScan_IgnorePatternSkipsMatchingFile verifies that a file whose absolute path matches
// an ignorePatterns entry is not dispatched and no dispatch error is recorded.
func TestScan_IgnorePatternSkipsMatchingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "video.mkv.!qB"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "video.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, IgnorePatterns: []CompiledRegexp{{Regexp: regexp.MustCompile(`\.!qB$`)}}, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var dispatched []string

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		dispatched = append(dispatched, input.FilePath)
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.Len(t, dispatched, 1)
	assert.Equal(t, filepath.Join(dir, "video.mkv"), dispatched[0])
}

// TestScan_IgnorePatternPrunesMatchingDirectory verifies that when a directory's absolute
// path matches an ignorePatterns entry, the directory and its entire subtree are skipped.
func TestScan_IgnorePatternPrunesMatchingDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	unpackDir := filepath.Join(dir, "_unpack")
	require.NoError(t, os.MkdirAll(unpackDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unpackDir, "video.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, IgnorePatterns: []CompiledRegexp{{Regexp: regexp.MustCompile(`(^|/)_unpack(/|$)`)}}, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var dispatched []string

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		dispatched = append(dispatched, input.FilePath)
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.Len(t, dispatched, 1)
	assert.Equal(t, filepath.Join(dir, "movie.mkv"), dispatched[0])
}

// TestScan_NonMatchingFileDispatchedWithIgnorePatterns verifies that a file whose absolute
// path does not match any ignorePatterns entry is dispatched normally alongside ignored files.
func TestScan_NonMatchingFileDispatchedWithIgnorePatterns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "partial.mkv.!qB"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, IgnorePatterns: []CompiledRegexp{{Regexp: regexp.MustCompile(`\.!qB$`)}}, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var dispatched []string

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		dispatched = append(dispatched, input.FilePath)
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.Len(t, dispatched, 1)
	assert.Equal(t, filepath.Join(dir, "movie.mkv"), dispatched[0])
}

// TestScan_DispatchErrorsCounter verifies that watcher_dispatch_errors_total increments
// by 1 when a dispatch call fails, with correct labels.
func TestScan_DispatchErrorsCounter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	_ = scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return errors.New("temporal unavailable")
	})

	mf := findMetricFamily(t, reg, "watcher_dispatch_errors_total")
	require.NotNil(t, mf)

	m := findCounter(mf, map[string]string{
		"mapping_name": "movies",
		"media_type":   string(medialib.MovieType),
	})
	require.NotNil(t, m, "expected data point with mapping_name=movies media_type=movie")
	assert.EqualValues(t, 1, m.GetCounter().GetValue())
}

// TestScan_AlreadyStartedNotCountedAsDispatchOrError verifies that a dispatch returning
// errWorkflowAlreadyStarted (the multi-watcher dedup case) is treated as a normal
// no-op: neither watcher_dispatches_total nor watcher_dispatch_errors_total increments,
// and the surrounding scan still completes with status=success.
func TestScan_AlreadyStartedNotCountedAsDispatchOrError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	instruments, reg := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ mediatypes.MediaInput) error {
		return errWorkflowAlreadyStarted
	}))

	dispatches := findMetricFamily(t, reg, "watcher_dispatches_total")
	assert.Nil(t, dispatches, "watcher_dispatches_total should not be emitted when dedup suppressed dispatch")

	dispatchErrors := findMetricFamily(t, reg, "watcher_dispatch_errors_total")
	assert.Nil(t, dispatchErrors, "watcher_dispatch_errors_total should not be emitted on dedup")

	scans := findMetricFamily(t, reg, "watcher_scans_total")
	require.NotNil(t, scans, "watcher_scans_total should be present")

	m := findCounter(scans, map[string]string{"mapping_name": "movies", "status": "success"})
	require.NotNil(t, m, "scan should be recorded as success when dedup suppresses dispatch")
	assert.EqualValues(t, 1, m.GetCounter().GetValue())
}

// TestWorkflowID_DeterministicAndPathSensitive verifies that workflowID is stable for
// the same path across calls and produces a different ID for a different path.
func TestWorkflowID_DeterministicAndPathSensitive(t *testing.T) {
	t.Parallel()

	a := workflowID("/watch/movies/movie.mkv")
	b := workflowID("/watch/movies/movie.mkv")
	c := workflowID("/watch/movies/other.mkv")

	assert.Equal(t, a, b, "same path should produce the same WorkflowID")
	assert.NotEqual(t, a, c, "different paths should produce different WorkflowIDs")
	assert.True(t, len(a) > 0 && len(a) < 1000, "WorkflowID should be non-empty and within Temporal's length limits")
	assert.Contains(t, a, "media-", "WorkflowID should carry the media- prefix for UI readability")
}

// TestScan_PreserveSourceForwardedToDispatch verifies that the preserveSource value from a
// WatchEntry is forwarded verbatim to the dispatch callback for each discovered file.
func TestScan_PreserveSourceForwardedToDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		preserveSource bool
	}{
		{name: "preserveSource true is forwarded", preserveSource: true},
		{name: "preserveSource false is forwarded", preserveSource: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

			cfg := &Config{
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, PreserveSource: tt.preserveSource, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
				},
			}

			var gotPreserveSource bool

			var dispatched bool

			dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
				gotPreserveSource = input.PreserveSource
				dispatched = true

				return nil
			}

			require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
			require.True(t, dispatched, "expected dispatch to be called")
			assert.Equal(t, tt.preserveSource, gotPreserveSource)
		})
	}
}

// TestScan_WatchRootForwardedToDispatch verifies that the watch entry's Path is forwarded
// as watchRoot to the dispatch callback for each discovered file.
func TestScan_WatchRootForwardedToDispatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var gotWatchRoot string

	var dispatched bool

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		gotWatchRoot = input.WatchRoot
		dispatched = true

		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.True(t, dispatched, "expected dispatch to be called")
	assert.Equal(t, dir, gotWatchRoot)
}

// TestScan_RetainEmptyDirsForwardedToDispatch verifies that the retainEmptyDirectories value
// from a WatchEntry is forwarded verbatim to the dispatch callback for each discovered file.
func TestScan_RetainEmptyDirsForwardedToDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		retainEmptyDirectories bool
	}{
		{name: "retainEmptyDirectories true is forwarded", retainEmptyDirectories: true},
		{name: "retainEmptyDirectories false is forwarded", retainEmptyDirectories: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

			cfg := &Config{
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, RetainEmptyDirectories: tt.retainEmptyDirectories, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
				},
			}

			var gotRetainEmptyDirs bool

			var dispatched bool

			dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
				gotRetainEmptyDirs = input.RetainEmptyDirectories
				dispatched = true

				return nil
			}

			require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
			require.True(t, dispatched, "expected dispatch to be called")
			assert.Equal(t, tt.retainEmptyDirectories, gotRetainEmptyDirs)
		})
	}
}

// TestScan_SkipsSentinelledFile verifies that a file is not dispatched when its
// corresponding sentinel (.BASENAME.done) already exists in the same directory.
func TestScan_SkipsSentinelledFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".movie.mkv.done"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var dispatched []string

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		dispatched = append(dispatched, input.FilePath)
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	assert.Empty(t, dispatched, "file with sentinel should not be dispatched")
}

// TestScan_SkipsSentinelFileItself verifies that a hidden .BASENAME.done sentinel file
// is not dispatched as a media file even when no ignore patterns are configured.
func TestScan_SkipsSentinelFileItself(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".movie.mkv.done"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: t.TempDir()}},
		},
	}

	var dispatched []string

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		dispatched = append(dispatched, input.FilePath)
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	assert.Empty(t, dispatched, "sentinel file itself should not be dispatched")
}

// TestScan_OutputPathForwardedToDispatch verifies that the watch entry's output.path is
// forwarded as an absolute path to the dispatch callback for each discovered file.
func TestScan_OutputPathForwardedToDispatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outputDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: outputDir}},
		},
	}

	var gotOutputPath string

	var dispatched bool

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		gotOutputPath = input.OutputPath
		dispatched = true

		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.True(t, dispatched, "expected dispatch to be called")

	absOutputDir, err := filepath.Abs(outputDir)
	require.NoError(t, err)
	assert.Equal(t, absOutputDir, gotOutputPath)
}

// TestScan_OutputRemotePathForwardedToDispatch verifies that the watch entry's output.remotePath
// is forwarded verbatim to the dispatch callback for each discovered file.
func TestScan_OutputRemotePathForwardedToDispatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", WatchedPath: dir, MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: dir, RemotePath: "/remote/movies"}},
		},
	}

	var gotOutputRemotePath string

	var dispatched bool

	dispatch := func(_ context.Context, input mediatypes.MediaInput) error {
		gotOutputRemotePath = input.OutputRemotePath
		dispatched = true

		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, noopInstruments(t), dispatch))
	require.True(t, dispatched, "expected dispatch to be called")
	assert.Equal(t, "/remote/movies", gotOutputRemotePath)
}
