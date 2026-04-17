package media

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// Recorder holds OTel instruments for the media workflow and records per-run observations.
type Recorder struct {
	highCardinality bool

	audioTrackCount          otelmetric.Float64Histogram
	subtitleTrackCount       otelmetric.Float64Histogram
	sourceDuration           otelmetric.Float64Histogram
	sourceFileSize           otelmetric.Float64Histogram
	destFileSize             otelmetric.Float64Histogram
	transcodeDuration        otelmetric.Float64Histogram
	totalDuration            otelmetric.Float64Histogram
	invalidFilesTotal        otelmetric.Int64Counter
	metricsErrorsTotal       otelmetric.Int64Counter
	artworkFetchSkippedTotal otelmetric.Int64Counter
}

// NewRecorder creates a Recorder that registers all media workflow instruments against mp.
// When highCardinality is true, RecordRun attaches id/title/year (and episode-specific)
// labels to every observation.
func NewRecorder(mp otelmetric.MeterProvider, highCardinality bool) (*Recorder, error) {
	meter := mp.Meter("media_workflow")

	audioTrackCount, err := meter.Float64Histogram("media_workflow_audio_track_count",
		otelmetric.WithDescription("Number of audio tracks in the source file"),
		otelmetric.WithUnit("{track}"))
	if err != nil {
		return nil, fmt.Errorf("create audio_track_count histogram: %w", err)
	}

	subtitleTrackCount, err := meter.Float64Histogram("media_workflow_subtitle_track_count",
		otelmetric.WithDescription("Number of subtitle tracks in the source file"),
		otelmetric.WithUnit("{track}"))
	if err != nil {
		return nil, fmt.Errorf("create subtitle_track_count histogram: %w", err)
	}

	sourceDuration, err := meter.Float64Histogram("media_workflow_source_duration_seconds",
		otelmetric.WithDescription("Duration of the source media file"),
		otelmetric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("create source_duration_seconds histogram: %w", err)
	}

	sourceFileSize, err := meter.Float64Histogram("media_workflow_source_file_size_bytes",
		otelmetric.WithDescription("Size of the source file before transcoding"),
		otelmetric.WithUnit("By"))
	if err != nil {
		return nil, fmt.Errorf("create source_file_size_bytes histogram: %w", err)
	}

	destFileSize, err := meter.Float64Histogram("media_workflow_destination_file_size_bytes",
		otelmetric.WithDescription("Size of the output file after transcoding"),
		otelmetric.WithUnit("By"))
	if err != nil {
		return nil, fmt.Errorf("create destination_file_size_bytes histogram: %w", err)
	}

	transcodeDuration, err := meter.Float64Histogram("media_workflow_transcode_duration_seconds",
		otelmetric.WithDescription("Wall-clock time spent in RunTranscode"),
		otelmetric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("create transcode_duration_seconds histogram: %w", err)
	}

	totalDuration, err := meter.Float64Histogram("media_workflow_total_duration_seconds",
		otelmetric.WithDescription("Wall-clock time from probe start to cleanup finish"),
		otelmetric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("create total_duration_seconds histogram: %w", err)
	}

	invalidFilesTotal, err := meter.Int64Counter("media_workflow_invalid_files_total",
		otelmetric.WithDescription("Number of files skipped because they are not valid media"))
	if err != nil {
		return nil, fmt.Errorf("create invalid_files_total counter: %w", err)
	}

	metricsErrorsTotal, err := meter.Int64Counter("media_workflow_metrics_errors_total",
		otelmetric.WithDescription("Number of errors encountered while collecting per-run metrics (e.g. GetInfo failures)"))
	if err != nil {
		return nil, fmt.Errorf("create metrics_errors_total counter: %w", err)
	}

	artworkFetchSkippedTotal, err := meter.Int64Counter("media_workflow_artwork_fetch_skipped_total",
		otelmetric.WithDescription("Number of transcode runs where artwork fetch was attempted but yielded no embeddable image"))
	if err != nil {
		return nil, fmt.Errorf("create artwork_fetch_skipped_total counter: %w", err)
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

// RecordRun records the full set of processing metrics for a completed valid-media workflow run.
// mediaInfo may be nil; when non-nil and highCardinality is enabled, per-item labels are added.
// hardwareAccelerated should be true when a hardware device path was configured.
// totalElapsed is the wall-clock time from probe start to cleanup finish.
func (r *Recorder) RecordRun(
	ctx context.Context,
	input MediaInput,
	probe steps.ProbeOutput,
	transcode steps.TranscodeOutput,
	mediaInfo medialib.MediaInfo,
	hardwareAccelerated bool,
	totalElapsed time.Duration,
) {
	attrs := buildStandardAttrs(input, probe, transcode, hardwareAccelerated)
	if r.highCardinality && mediaInfo != nil {
		attrs = append(attrs, highCardinalityAttrs(input.MediaType, mediaInfo)...)
	}

	opts := otelmetric.WithAttributes(attrs...)

	r.audioTrackCount.Record(ctx, float64(len(probe.AudioStreams)), opts)
	r.subtitleTrackCount.Record(ctx, float64(len(probe.SubtitleStreams)), opts)
	r.sourceDuration.Record(ctx, probe.DurationSeconds, opts)
	r.sourceFileSize.Record(ctx, float64(transcode.SourceFileSizeBytes), opts)
	r.destFileSize.Record(ctx, float64(transcode.DestFileSizeBytes), opts)
	r.transcodeDuration.Record(ctx, transcode.TranscodeDurationSeconds, opts)
	r.totalDuration.Record(ctx, totalElapsed.Seconds(), opts)
}

// RecordInvalidFile increments the invalid_files_total counter. Only media_type and
// mapping_name labels are applied (no transcode data is available for invalid files).
func (r *Recorder) RecordInvalidFile(ctx context.Context, mediaType medialib.MediaType, mappingName string) {
	r.invalidFilesTotal.Add(ctx, 1,
		otelmetric.WithAttributes(
			mediaTypeAttr(mediaType),
			mappingNameAttr(mappingName),
		),
	)
}

// RecordArtworkFetchSkipped increments the artwork_fetch_skipped_total counter.
func (r *Recorder) RecordArtworkFetchSkipped(ctx context.Context) {
	r.artworkFetchSkippedTotal.Add(ctx, 1)
}

// RecordMetricsError increments the metrics_errors_total counter and logs the error.
func (r *Recorder) RecordMetricsError(ctx context.Context, err error) {
	slog.Warn("media workflow metrics error", "error", err)
	r.metricsErrorsTotal.Add(ctx, 1)
}
