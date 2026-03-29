package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// noopInstruments returns a scanInstruments backed by a no-op MeterProvider.
func noopInstruments(t *testing.T) *scanInstruments {
	t.Helper()
	inst, err := newScanInstruments(noop.NewMeterProvider())
	require.NoError(t, err)
	return inst
}

// newTestInstruments returns a scanInstruments backed by a ManualReader so metric
// values can be inspected in acceptance tests.
func newTestInstruments(t *testing.T) (*scanInstruments, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })

	inst, err := newScanInstruments(provider)
	require.NoError(t, err)
	return inst, reader
}

// collectMetrics gathers all current metric data from the reader.
func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	return rm
}

// findMetric returns the first Metrics entry whose Name matches name, or nil.
func findMetric(rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}

// attrKey converts a label name to an OTel attribute.Key for use in test assertions.
func attrKey(name string) attribute.Key { return attribute.Key(name) }

// findCounterDP returns the int64 sum data point whose attributes match the given
// mapping_name (and optionally status/media_type), or nil if not found.
func findCounterDP(dps []metricdata.DataPoint[int64], attrs map[string]string) *metricdata.DataPoint[int64] {
	for i := range dps {
		match := true
		for k, v := range attrs {
			val, ok := dps[i].Attributes.Value(attrKey(k))
			if !ok || val.AsString() != v {
				match = false
				break
			}
		}
		if match {
			return &dps[i]
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
					{Name: "movies", Path: t.TempDir(), MediaType: medialib.MovieType},
				},
			},
			errFunc: require.NoError,
		},
		{
			name: "missing directory returns error",
			cfg: &Config{
				Watches: []WatchEntry{
					{Name: "movies", Path: "/nonexistent/path/abc123", MediaType: medialib.MovieType},
				},
			},
			errFunc: require.Error,
		},
		{
			name: "all errors reported when multiple dirs are missing",
			cfg: &Config{
				Watches: []WatchEntry{
					{Name: "alpha", Path: "/nonexistent/alpha", MediaType: medialib.MovieType},
					{Name: "beta", Path: "/nonexistent/beta", MediaType: medialib.MovieType},
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
				return &Config{Watches: []WatchEntry{{Name: "movies", Path: f.Name(), MediaType: medialib.MovieType}}}
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
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	type call struct {
		filePath    string
		mediaType   medialib.MediaType
		mappingName string
	}
	var calls []call
	dispatch := func(_ context.Context, fp string, mt medialib.MediaType, mn string) error {
		calls = append(calls, call{fp, mt, mn})
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
			{Name: "shows", Path: dir, MediaType: medialib.ShowType},
		},
	}

	var dispatched []string
	dispatch := func(_ context.Context, fp string, _ medialib.MediaType, _ string) error {
		dispatched = append(dispatched, fp)
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
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	var count int
	dispatch := func(_ context.Context, _ string, _ medialib.MediaType, _ string) error {
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
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately before scan starts

	err := scan(ctx, cfg, noopInstruments(t), func(_ context.Context, _ string, _ medialib.MediaType, _ string) error { return nil })
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
			{Name: "movies", Path: movieDir, MediaType: medialib.MovieType},
			{Name: "shows", Path: showDir, MediaType: medialib.ShowType},
		},
	}

	dispatched := make(map[string]medialib.MediaType) // path → media type
	dispatch := func(_ context.Context, fp string, mt medialib.MediaType, _ string) error {
		dispatched[fp] = mt
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
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	instruments, reader := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error { return nil }))

	rm := collectMetrics(t, reader)

	for _, name := range []string{
		"watcher_scans_total",
		"watcher_scan_duration_seconds",
		"watcher_last_successful_scan_unix_seconds",
	} {
		assert.NotNil(t, findMetric(rm, name), "expected metric %q to be present", name)
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
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	instruments, reader := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error { return nil }))

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "watcher_scans_total")
	require.NotNil(t, m, "watcher_scans_total should be present")

	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)

	dp := findCounterDP(sum.DataPoints, map[string]string{"mapping_name": "movies", "status": "success"})
	require.NotNil(t, dp, "expected data point with mapping_name=movies status=success")
	assert.EqualValues(t, 1, dp.Value)
}

// TestScan_ErrorCounterIncrements verifies that watcher_scans_total{status="error",
// mapping_name="..."} increments when a mapping's walk or dispatch produces at least one error.
func TestScan_ErrorCounterIncrements(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	instruments, reader := newTestInstruments(t)
	_ = scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error {
		return errors.New("simulated dispatch failure")
	})

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "watcher_scans_total")
	require.NotNil(t, m)

	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)

	dp := findCounterDP(sum.DataPoints, map[string]string{"mapping_name": "movies", "status": "error"})
	require.NotNil(t, dp, "expected data point with mapping_name=movies status=error")
	assert.EqualValues(t, 1, dp.Value)
}

// TestScan_DurationObservedPerMapping verifies that watcher_scan_duration_seconds
// records one observation per mapping, labelled with mapping_name.
func TestScan_DurationObservedPerMapping(t *testing.T) {
	t.Parallel()

	movieDir := t.TempDir()
	showDir := t.TempDir()

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", Path: movieDir, MediaType: medialib.MovieType},
			{Name: "shows", Path: showDir, MediaType: medialib.ShowType},
		},
	}

	instruments, reader := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error { return nil }))

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "watcher_scan_duration_seconds")
	require.NotNil(t, m)

	h, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, h.DataPoints, 2, "expected one histogram data point per mapping")

	mappingNames := make(map[string]bool)
	for _, dp := range h.DataPoints {
		val, ok := dp.Attributes.Value(attrKey("mapping_name"))
		require.True(t, ok, "mapping_name label should be present")
		mappingNames[val.AsString()] = true
		assert.EqualValues(t, 1, dp.Count, "each mapping should have exactly one observation")
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
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	instruments, reader := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error { return nil }))

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "watcher_last_successful_scan_unix_seconds")
	require.NotNil(t, m)

	g, ok := m.Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	require.Len(t, g.DataPoints, 1)
	assert.Greater(t, g.DataPoints[0].Value, float64(0), "last successful scan timestamp should be non-zero")
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
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	instruments, reader := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error { return nil }))

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "watcher_files_discovered_total")
	require.NotNil(t, m)

	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)

	dp := findCounterDP(sum.DataPoints, map[string]string{
		"mapping_name": "movies",
		"media_type":   string(medialib.MovieType),
	})
	require.NotNil(t, dp, "expected data point with mapping_name=movies media_type=movie")
	assert.EqualValues(t, 2, dp.Value)
}

// TestScan_DispatchesTotalCounter verifies that watcher_dispatches_total increments by 1
// per successful dispatch with correct mapping_name and media_type labels.
func TestScan_DispatchesTotalCounter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	instruments, reader := newTestInstruments(t)
	require.NoError(t, scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error { return nil }))

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "watcher_dispatches_total")
	require.NotNil(t, m)

	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)

	dp := findCounterDP(sum.DataPoints, map[string]string{
		"mapping_name": "movies",
		"media_type":   string(medialib.MovieType),
	})
	require.NotNil(t, dp, "expected data point with mapping_name=movies media_type=movie")
	assert.EqualValues(t, 1, dp.Value)
}

// TestScan_DispatchErrorsCounter verifies that watcher_dispatch_errors_total increments
// by 1 when a dispatch call to Hatchet fails, with correct labels.
func TestScan_DispatchErrorsCounter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Name: "movies", Path: dir, MediaType: medialib.MovieType},
		},
	}

	instruments, reader := newTestInstruments(t)
	_ = scan(t.Context(), cfg, instruments, func(_ context.Context, _ string, _ medialib.MediaType, _ string) error {
		return errors.New("hatchet unavailable")
	})

	rm := collectMetrics(t, reader)
	m := findMetric(rm, "watcher_dispatch_errors_total")
	require.NotNil(t, m)

	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)

	dp := findCounterDP(sum.DataPoints, map[string]string{
		"mapping_name": "movies",
		"media_type":   string(medialib.MovieType),
	})
	require.NotNil(t, dp, "expected data point with mapping_name=movies media_type=movie")
	assert.EqualValues(t, 1, dp.Value)
}
