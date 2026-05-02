package media

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// Histogram bucket boundaries chosen to span the typical operating range of
// each metric. Single source of truth so test buckets stay aligned with code.
var (
	trackCountBuckets = []float64{1, 2, 3, 4, 5, 8, 12, 20}
	durationBuckets   = []float64{60, 300, 600, 1800, 3600, 7200, 14400, 28800}
	fileSizeBuckets   = []float64{
		100_000_000, 500_000_000, 1_000_000_000, 5_000_000_000,
		10_000_000_000, 25_000_000_000, 50_000_000_000, 100_000_000_000,
	}
)

// Standard label set carried by every per-run histogram observation.
var standardLabels = []string{
	"media_type",
	"mapping_name",
	"source_codec",
	"destination_codec",
	"source_container",
	"destination_container",
	"hardware_accelerated",
	"crop_applied",
}

// Additional labels appended when high-cardinality mode is enabled. Kept as
// a fixed superset (id/title/year + episode-specific) so a single
// HistogramVec covers both movies and shows; episode-specific fields are
// empty strings for movie observations.
var highCardinalityLabels = []string{
	"id",
	"title",
	"year",
	"series_title",
	"season_number",
	"episode_number",
}

// Recorder holds Prometheus collectors for the media workflow and records per-run observations.
type Recorder struct {
	highCardinality bool

	audioTrackCount          *prometheus.HistogramVec
	subtitleTrackCount       *prometheus.HistogramVec
	sourceDuration           *prometheus.HistogramVec
	sourceFileSize           *prometheus.HistogramVec
	destFileSize             *prometheus.HistogramVec
	transcodeDuration        *prometheus.HistogramVec
	totalDuration            *prometheus.HistogramVec
	invalidFilesTotal        *prometheus.CounterVec
	metricsErrorsTotal       prometheus.Counter
	artworkFetchSkippedTotal prometheus.Counter
}

// NewRecorder creates a Recorder that registers all media workflow collectors against reg.
// When highCardinality is true, the per-run histograms carry id/title/year (and
// episode-specific) labels in addition to the standard label set.
func NewRecorder(reg prometheus.Registerer, highCardinality bool) (*Recorder, error) {
	labels := standardLabels
	if highCardinality {
		labels = make([]string, 0, len(standardLabels)+len(highCardinalityLabels))
		labels = append(labels, standardLabels...)
		labels = append(labels, highCardinalityLabels...)
	}

	mkHist := func(name, help, unit string, buckets []float64) (*prometheus.HistogramVec, error) {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    name,
			Help:    help + " (" + unit + ")",
			Buckets: buckets,
		}, labels)
		if err := reg.Register(h); err != nil {
			return nil, fmt.Errorf("register %s: %w", name, err)
		}

		return h, nil
	}

	audioTrackCount, err := mkHist("media_workflow_audio_track_count",
		"Number of audio tracks in the source file", "tracks", trackCountBuckets)
	if err != nil {
		return nil, err
	}

	subtitleTrackCount, err := mkHist("media_workflow_subtitle_track_count",
		"Number of subtitle tracks in the source file", "tracks", trackCountBuckets)
	if err != nil {
		return nil, err
	}

	sourceDuration, err := mkHist("media_workflow_source_duration_seconds",
		"Duration of the source media file", "seconds", durationBuckets)
	if err != nil {
		return nil, err
	}

	sourceFileSize, err := mkHist("media_workflow_source_file_size_bytes",
		"Size of the source file before transcoding", "bytes", fileSizeBuckets)
	if err != nil {
		return nil, err
	}

	destFileSize, err := mkHist("media_workflow_destination_file_size_bytes",
		"Size of the output file after transcoding", "bytes", fileSizeBuckets)
	if err != nil {
		return nil, err
	}

	transcodeDuration, err := mkHist("media_workflow_transcode_duration_seconds",
		"Wall-clock time spent in RunTranscode", "seconds", durationBuckets)
	if err != nil {
		return nil, err
	}

	totalDuration, err := mkHist("media_workflow_total_duration_seconds",
		"Wall-clock time from probe start to cleanup finish", "seconds", durationBuckets)
	if err != nil {
		return nil, err
	}

	invalidFilesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "media_workflow_invalid_files_total",
		Help: "Number of files skipped because they are not valid media",
	}, []string{"media_type", "mapping_name"})
	if err := reg.Register(invalidFilesTotal); err != nil {
		return nil, fmt.Errorf("register media_workflow_invalid_files_total: %w", err)
	}

	metricsErrorsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "media_workflow_metrics_errors_total",
		Help: "Number of errors encountered while collecting per-run metrics (e.g. GetInfo failures)",
	})
	if err := reg.Register(metricsErrorsTotal); err != nil {
		return nil, fmt.Errorf("register media_workflow_metrics_errors_total: %w", err)
	}

	artworkFetchSkippedTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "media_workflow_artwork_fetch_skipped_total",
		Help: "Number of transcode runs where artwork fetch was attempted but yielded no embeddable image",
	})
	if err := reg.Register(artworkFetchSkippedTotal); err != nil {
		return nil, fmt.Errorf("register media_workflow_artwork_fetch_skipped_total: %w", err)
	}

	return &Recorder{
		highCardinality:          highCardinality,
		audioTrackCount:          audioTrackCount,
		subtitleTrackCount:       subtitleTrackCount,
		sourceDuration:           sourceDuration,
		sourceFileSize:           sourceFileSize,
		destFileSize:             destFileSize,
		transcodeDuration:        transcodeDuration,
		totalDuration:            totalDuration,
		invalidFilesTotal:        invalidFilesTotal,
		metricsErrorsTotal:       metricsErrorsTotal,
		artworkFetchSkippedTotal: artworkFetchSkippedTotal,
	}, nil
}

// runLabels assembles the label values for a per-run histogram observation in
// the same order as the registered label set (standardLabels [+ highCardinalityLabels]).
func (r *Recorder) runLabels(input MediaInput, probe steps.ProbeOutput, transcode steps.TranscodeOutput, mediaInfo medialib.MediaInfo, hardwareAccelerated bool) prometheus.Labels {
	labels := prometheus.Labels{
		"media_type":            string(input.MediaType),
		"mapping_name":          input.MappingName,
		"source_codec":          probe.VideoCodec,
		"destination_codec":     transcode.DestCodec,
		"source_container":      probe.Format,
		"destination_container": transcode.DestContainer,
		"hardware_accelerated":  strconv.FormatBool(hardwareAccelerated),
		"crop_applied":          strconv.FormatBool(transcode.CropApplied),
	}

	if !r.highCardinality {
		return labels
	}

	if mediaInfo == nil {
		// HC is enabled but per-item info wasn't available; fill empty strings
		// so the label set stays consistent with the registered Vec.
		for _, k := range highCardinalityLabels {
			labels[k] = ""
		}

		return labels
	}

	labels["id"] = strconv.FormatInt(mediaInfo.GetID(), 10)
	labels["title"] = mediaInfo.GetTitle()
	labels["year"] = strconv.Itoa(mediaInfo.GetYear())

	if input.MediaType == medialib.ShowType {
		labels["series_title"] = mediaInfo.GetSeriesTitle()
		labels["season_number"] = strconv.Itoa(mediaInfo.GetSeasonNumber())
		labels["episode_number"] = strconv.Itoa(mediaInfo.GetEpisodeNumber())
	} else {
		labels["series_title"] = ""
		labels["season_number"] = ""
		labels["episode_number"] = ""
	}

	return labels
}

// RecordRun records the full set of processing metrics for a completed valid-media workflow run.
// mediaInfo may be nil; when non-nil and highCardinality is enabled, per-item labels are added.
// hardwareAccelerated should be true when at least one video stream was hardware-encoded.
// totalElapsed is the wall-clock time from probe start to cleanup finish.
func (r *Recorder) RecordRun(
	input MediaInput,
	probe steps.ProbeOutput,
	transcode steps.TranscodeOutput,
	mediaInfo medialib.MediaInfo,
	hardwareAccelerated bool,
	totalElapsed time.Duration,
) {
	labels := r.runLabels(input, probe, transcode, mediaInfo, hardwareAccelerated)

	r.audioTrackCount.With(labels).Observe(float64(len(probe.AudioStreams)))
	r.subtitleTrackCount.With(labels).Observe(float64(len(probe.SubtitleStreams)))
	r.sourceDuration.With(labels).Observe(probe.DurationSeconds)
	r.sourceFileSize.With(labels).Observe(float64(transcode.SourceFileSizeBytes))
	r.destFileSize.With(labels).Observe(float64(transcode.DestFileSizeBytes))
	r.transcodeDuration.With(labels).Observe(transcode.TranscodeDurationSeconds)
	r.totalDuration.With(labels).Observe(totalElapsed.Seconds())
}

// RecordInvalidFile increments the invalid_files_total counter. Only media_type and
// mapping_name labels are applied (no transcode data is available for invalid files).
func (r *Recorder) RecordInvalidFile(mediaType medialib.MediaType, mappingName string) {
	r.invalidFilesTotal.WithLabelValues(string(mediaType), mappingName).Inc()
}

// RecordArtworkFetchSkipped increments the artwork_fetch_skipped_total counter.
func (r *Recorder) RecordArtworkFetchSkipped() {
	r.artworkFetchSkippedTotal.Inc()
}

// RecordMetricsError increments the metrics_errors_total counter and logs the error.
func (r *Recorder) RecordMetricsError(err error) {
	slog.Warn("media workflow metrics error", "error", err)
	r.metricsErrorsTotal.Inc()
}
