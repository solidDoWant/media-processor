package media

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// newTestRecorder creates a Recorder backed by a fresh prometheus.Registry and returns both.
func newTestRecorder(t *testing.T, highCardinality bool) (*Recorder, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()

	rec, err := NewRecorder(reg, highCardinality)
	require.NoError(t, err)

	return rec, reg
}

// findMetricFamily returns the *dto.MetricFamily whose name matches, or nil if absent.
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
	rec, reg := newTestRecorder(t, false)

	rec.RecordRun(sampleInput(medialib.MovieType), sampleProbe(), sampleTranscode(), nil, false, 5*time.Second)

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
		mf := findMetricFamily(t, reg, name)
		require.NotNil(t, mf, "expected metric %q to be present", name)
		require.Len(t, mf.GetMetric(), 1, "metric %q should have exactly one series", name)
		assert.EqualValues(t, 1, mf.GetMetric()[0].GetHistogram().GetSampleCount(), "metric %q should have one observation", name)
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
			rec, reg := newTestRecorder(t, false)

			transcode := sampleTranscode()
			transcode.CropApplied = tc.cropApplied

			rec.RecordRun(sampleInput(medialib.MovieType), sampleProbe(), transcode, nil, false, 5*time.Second)

			mf := findMetricFamily(t, reg, "media_workflow_audio_track_count")
			require.NotNil(t, mf)
			require.Len(t, mf.GetMetric(), 1)

			val, present := labelValue(mf.GetMetric()[0], "crop_applied")
			assert.True(t, present, "crop_applied label should be present")
			assert.Equal(t, tc.wantCropApplied, val)
		})
	}
}

func TestRecorder_HardwareAcceleratedLabel(t *testing.T) {
	tests := []struct {
		name                    string
		hardwareAccelerated     bool
		wantHardwareAccelerated string
	}{
		{
			name:                    "software encoder — label is false",
			hardwareAccelerated:     false,
			wantHardwareAccelerated: "false",
		},
		{
			name:                    "hardware encoder — label is true",
			hardwareAccelerated:     true,
			wantHardwareAccelerated: "true",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, reg := newTestRecorder(t, false)

			transcode := sampleTranscode()
			transcode.HardwareAccelerated = tc.hardwareAccelerated

			rec.RecordRun(sampleInput(medialib.MovieType), sampleProbe(), transcode, nil, tc.hardwareAccelerated, 5*time.Second)

			mf := findMetricFamily(t, reg, "media_workflow_audio_track_count")
			require.NotNil(t, mf)
			require.Len(t, mf.GetMetric(), 1)

			val, present := labelValue(mf.GetMetric()[0], "hardware_accelerated")
			assert.True(t, present, "hardware_accelerated label should be present")
			assert.Equal(t, tc.wantHardwareAccelerated, val)
		})
	}
}

func TestRecorder_InvalidFileRecordsOnlyCounter(t *testing.T) {
	rec, reg := newTestRecorder(t, false)

	rec.RecordInvalidFile(medialib.MovieType, "test-mapping")

	// Counter must be present with correct labels.
	mf := findMetricFamily(t, reg, "media_workflow_invalid_files_total")
	require.NotNil(t, mf, "invalid_files_total counter should be present")
	require.Len(t, mf.GetMetric(), 1)
	m := mf.GetMetric()[0]
	assert.EqualValues(t, 1, m.GetCounter().GetValue())

	mediaTypeVal, hasMT := labelValue(m, "media_type")
	assert.True(t, hasMT, "media_type label should be present")
	assert.Equal(t, string(medialib.MovieType), mediaTypeVal)

	mappingVal, hasMN := labelValue(m, "mapping_name")
	assert.True(t, hasMN, "mapping_name label should be present")
	assert.Equal(t, "test-mapping", mappingVal)

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
		assert.Nil(t, findMetricFamily(t, reg, name), "processing metric %q should not be present for invalid file", name)
	}
}

func TestRecorder_LowCardinalityMode_NoHighCardinalityLabels(t *testing.T) {
	rec, reg := newTestRecorder(t, false)

	movie := &medialib.Movie{ID: 42, Title: "Test Movie", Year: 2020}
	rec.RecordRun(sampleInput(medialib.MovieType), sampleProbe(), sampleTranscode(), movie, false, 5*time.Second)

	mf := findMetricFamily(t, reg, "media_workflow_audio_track_count")
	require.NotNil(t, mf)
	require.Len(t, mf.GetMetric(), 1)

	highCardKeys := []string{"id", "title", "year", "series_title", "season_number", "episode_number"}
	for _, key := range highCardKeys {
		_, present := labelValue(mf.GetMetric()[0], key)
		assert.False(t, present, "high-cardinality label %q should not be present when disabled", key)
	}
}

func TestRecorder_HighCardinalityMode_MovieLabels(t *testing.T) {
	rec, reg := newTestRecorder(t, true)

	movie := &medialib.Movie{ID: 42, Title: "Test Movie", Year: 2020}
	rec.RecordRun(sampleInput(medialib.MovieType), sampleProbe(), sampleTranscode(), movie, false, 5*time.Second)

	mf := findMetricFamily(t, reg, "media_workflow_audio_track_count")
	require.NotNil(t, mf)
	require.Len(t, mf.GetMetric(), 1)
	m := mf.GetMetric()[0]

	idVal, hasID := labelValue(m, "id")
	assert.True(t, hasID, "id label should be present")
	assert.Equal(t, "42", idVal)

	titleVal, hasTitle := labelValue(m, "title")
	assert.True(t, hasTitle, "title label should be present")
	assert.Equal(t, "Test Movie", titleVal)

	yearVal, hasYear := labelValue(m, "year")
	assert.True(t, hasYear, "year label should be present")
	assert.Equal(t, "2020", yearVal)

	// Episode-specific labels are present in HC mode but empty for movies, so the
	// HistogramVec's fixed label set stays consistent across observations.
	for _, key := range []string{"series_title", "season_number", "episode_number"} {
		val, present := labelValue(m, key)
		assert.True(t, present, "episode label %q should be present in HC mode (empty for movies)", key)
		assert.Equal(t, "", val, "episode label %q should be empty for a movie", key)
	}
}

func TestRecorder_HighCardinalityMode_EpisodeLabels(t *testing.T) {
	rec, reg := newTestRecorder(t, true)

	episode := &medialib.Episode{
		ID:            99,
		Title:         "Pilot",
		Year:          2019,
		SeriesTitle:   "Test Show",
		SeasonNumber:  1,
		EpisodeNumber: 1,
	}
	rec.RecordRun(sampleInput(medialib.ShowType), sampleProbe(), sampleTranscode(), episode, false, 5*time.Second)

	mf := findMetricFamily(t, reg, "media_workflow_audio_track_count")
	require.NotNil(t, mf)
	require.Len(t, mf.GetMetric(), 1)
	m := mf.GetMetric()[0]

	expected := map[string]string{
		"id":             "99",
		"title":          "Pilot",
		"year":           "2019",
		"series_title":   "Test Show",
		"season_number":  "1",
		"episode_number": "1",
	}
	for key, want := range expected {
		val, present := labelValue(m, key)
		assert.True(t, present, "label %q should be present for an episode", key)
		assert.Equal(t, want, val, "label %q value mismatch", key)
	}
}
