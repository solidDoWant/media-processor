package media

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// attributeKey converts a label name to an OTel attribute.Key for use in test assertions.
func attributeKey(name string) attribute.Key { return attribute.Key(name) }

// newTestRecorder creates a Recorder backed by a ManualReader and returns both.
func newTestRecorder(t *testing.T, highCardinality bool) (*Recorder, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })

	rec, err := NewRecorder(provider, highCardinality)
	require.NoError(t, err)

	return rec, reader
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

// histogramDataPoints returns all data points from a Float64Histogram metric.
func histogramDataPoints(t *testing.T, m *metricdata.Metrics) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	require.NotNil(t, m, "expected metric to be present")
	h, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected metric %q to be a float64 histogram", m.Name)

	return h.DataPoints
}

// sampleInput returns a MediaInput suitable for testing.
func sampleInput(mt medialib.MediaType) MediaInput {
	return MediaInput{
		FilePath:    "/media/test.mp4",
		MediaType:   mt,
		MappingName: "test-mapping",
	}
}

// sampleProbe returns a ProbeOutput suitable for testing.
func sampleProbe() steps.ProbeOutput {
	return steps.ProbeOutput{
		IsValidMedia:    true,
		VideoCodec:      "h264",
		Format:          "mov,mp4,m4a,3gp,3g2,mj2",
		DurationSeconds: 120.5,
		AudioStreams: []steps.AudioStreamInfo{
			{StreamInfo: steps.StreamInfo{Index: 1, Language: "eng"}, ReportedChannelCount: 2, EffectiveChannelCount: 2},
			{StreamInfo: steps.StreamInfo{Index: 2, Language: "eng"}, ReportedChannelCount: 6, EffectiveChannelCount: 6},
		},
		SubtitleStreams: []steps.StreamInfo{
			{Index: 3, Language: "eng"},
		},
		StartedAt: time.Now().Add(-5 * time.Second),
	}
}

// sampleTranscode returns a TranscodeOutput suitable for testing.
func sampleTranscode() steps.TranscodeOutput {
	return steps.TranscodeOutput{
		DestCodec:                "hevc",
		DestContainer:            "mkv",
		DestFilePath:             "/output/test.mkv",
		SourceFileSizeBytes:      100_000_000,
		DestFileSizeBytes:        80_000_000,
		TranscodeDurationSeconds: 30.0,
	}
}

func TestRecorder_ValidRunRecordsAllProcessingHistograms(t *testing.T) {
	rec, reader := newTestRecorder(t, false)

	rec.RecordRun(t.Context(), sampleInput(medialib.MovieType), sampleProbe(), sampleTranscode(), nil, false, 5*time.Second)

	rm := collectMetrics(t, reader)

	processingMetrics := []string{
		"media_workflow_audio_track_count",
		"media_workflow_subtitle_track_count",
		"media_workflow_source_duration_seconds",
		"media_workflow_source_file_size_bytes",
		"media_workflow_destination_file_size_bytes",
		"media_workflow_transcode_duration_seconds",
		"media_workflow_total_duration_seconds",
	}
	for _, name := range processingMetrics {
		m := findMetric(rm, name)
		require.NotNil(t, m, "expected metric %q to be present", name)
		dps := histogramDataPoints(t, m)
		assert.Len(t, dps, 1, "metric %q should have exactly one data point", name)
	}
}

func TestRecorder_CropAppliedLabel(t *testing.T) {
	tests := []struct {
		name            string
		cropApplied     bool
		wantCropApplied string
	}{
		{
			name:            "crop not applied — label is false",
			cropApplied:     false,
			wantCropApplied: "false",
		},
		{
			name:            "crop applied — label is true",
			cropApplied:     true,
			wantCropApplied: "true",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, reader := newTestRecorder(t, false)

			transcode := sampleTranscode()
			transcode.CropApplied = tc.cropApplied

			rec.RecordRun(t.Context(), sampleInput(medialib.MovieType), sampleProbe(), transcode, nil, false, 5*time.Second)

			rm := collectMetrics(t, reader)

			m := findMetric(rm, "media_workflow_audio_track_count")
			require.NotNil(t, m)
			dps := histogramDataPoints(t, m)
			require.Len(t, dps, 1)

			val, present := dps[0].Attributes.Value(attributeKey("crop_applied"))
			assert.True(t, present, "crop_applied label should be present")
			assert.Equal(t, tc.wantCropApplied, val.AsString())
		})
	}
}

func TestRecorder_InvalidFileRecordsOnlyCounter(t *testing.T) {
	rec, reader := newTestRecorder(t, false)

	rec.RecordInvalidFile(t.Context(), medialib.MovieType, "test-mapping")

	rm := collectMetrics(t, reader)

	// Counter must be present with correct labels.
	m := findMetric(rm, "media_workflow_invalid_files_total")
	require.NotNil(t, m, "invalid_files_total counter should be present")
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "invalid_files_total should be an int64 sum/counter")
	require.Len(t, sum.DataPoints, 1)
	dp := sum.DataPoints[0]
	assert.EqualValues(t, 1, dp.Value)

	mediaTypeVal, hasMT := dp.Attributes.Value(attributeKey("media_type"))
	assert.True(t, hasMT, "media_type label should be present")
	assert.Equal(t, string(medialib.MovieType), mediaTypeVal.AsString())

	mappingVal, hasMN := dp.Attributes.Value(attributeKey("mapping_name"))
	assert.True(t, hasMN, "mapping_name label should be present")
	assert.Equal(t, "test-mapping", mappingVal.AsString())

	// None of the processing histograms should be present.
	processingMetrics := []string{
		"media_workflow_audio_track_count",
		"media_workflow_subtitle_track_count",
		"media_workflow_source_duration_seconds",
		"media_workflow_source_file_size_bytes",
		"media_workflow_destination_file_size_bytes",
		"media_workflow_transcode_duration_seconds",
		"media_workflow_total_duration_seconds",
	}
	for _, name := range processingMetrics {
		assert.Nil(t, findMetric(rm, name), "processing metric %q should not be present for invalid file", name)
	}
}

func TestRecorder_LowCardinalityMode_NoHighCardinalityLabels(t *testing.T) {
	rec, reader := newTestRecorder(t, false)

	movie := &medialib.Movie{ID: 42, Title: "Test Movie", Year: 2020}
	rec.RecordRun(t.Context(), sampleInput(medialib.MovieType), sampleProbe(), sampleTranscode(), movie, false, 5*time.Second)

	rm := collectMetrics(t, reader)

	m := findMetric(rm, "media_workflow_audio_track_count")
	require.NotNil(t, m)
	dps := histogramDataPoints(t, m)
	require.Len(t, dps, 1)

	highCardKeys := []string{"id", "title", "year", "series_title", "season_number", "episode_number"}
	for _, key := range highCardKeys {
		_, present := dps[0].Attributes.Value(attributeKey(key))
		assert.False(t, present, "high-cardinality label %q should not be present when disabled", key)
	}
}

func TestRecorder_HighCardinalityMode_MovieLabels(t *testing.T) {
	rec, reader := newTestRecorder(t, true)

	movie := &medialib.Movie{ID: 42, Title: "Test Movie", Year: 2020}
	rec.RecordRun(t.Context(), sampleInput(medialib.MovieType), sampleProbe(), sampleTranscode(), movie, false, 5*time.Second)

	rm := collectMetrics(t, reader)

	m := findMetric(rm, "media_workflow_audio_track_count")
	require.NotNil(t, m)
	dps := histogramDataPoints(t, m)
	require.Len(t, dps, 1)
	dp := dps[0]

	idVal, hasID := dp.Attributes.Value(attributeKey("id"))
	assert.True(t, hasID, "id label should be present")
	assert.Equal(t, "42", idVal.AsString())

	titleVal, hasTitle := dp.Attributes.Value(attributeKey("title"))
	assert.True(t, hasTitle, "title label should be present")
	assert.Equal(t, "Test Movie", titleVal.AsString())

	yearVal, hasYear := dp.Attributes.Value(attributeKey("year"))
	assert.True(t, hasYear, "year label should be present")
	assert.Equal(t, "2020", yearVal.AsString())

	// Episode-only labels must be absent for movies.
	for _, key := range []string{"series_title", "season_number", "episode_number"} {
		_, present := dp.Attributes.Value(attributeKey(key))
		assert.False(t, present, "episode label %q should not be present for a movie", key)
	}
}

func TestRecorder_HighCardinalityMode_EpisodeLabels(t *testing.T) {
	rec, reader := newTestRecorder(t, true)

	episode := &medialib.Episode{
		ID:            99,
		Title:         "Pilot",
		Year:          2019,
		SeriesTitle:   "Test Show",
		SeasonNumber:  1,
		EpisodeNumber: 1,
	}
	rec.RecordRun(t.Context(), sampleInput(medialib.ShowType), sampleProbe(), sampleTranscode(), episode, false, 5*time.Second)

	rm := collectMetrics(t, reader)

	m := findMetric(rm, "media_workflow_audio_track_count")
	require.NotNil(t, m)
	dps := histogramDataPoints(t, m)
	require.Len(t, dps, 1)
	dp := dps[0]

	expected := map[string]string{
		"id":             "99",
		"title":          "Pilot",
		"year":           "2019",
		"series_title":   "Test Show",
		"season_number":  "1",
		"episode_number": "1",
	}
	for key, want := range expected {
		val, present := dp.Attributes.Value(attributeKey(key))
		assert.True(t, present, "label %q should be present for an episode", key)
		assert.Equal(t, want, val.AsString(), "label %q value mismatch", key)
	}
}
