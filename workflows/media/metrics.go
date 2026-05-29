package media

import (
	"strconv"
	"time"

	"github.com/uber-go/tally/v4"

	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
)

// Bucket boundaries for histogram metrics. Factor-2 exponential sweeps cover
// the operating ranges with one bucket per power of two — fine enough for
// PromQL quantile interpolation, broad enough to span the three orders of
// magnitude these metrics see in practice.
var (
	// fileSizeBuckets: 10 MB → ~328 GB, doubling each step (16 buckets).
	// Spans short clips through oversized 4K HDR remuxes; the top boundary
	// is sized so that 200 GB observations fall in an explicit bucket
	// (not +Inf) and quantile estimates remain meaningful.
	fileSizeBuckets = tally.MustMakeExponentialValueBuckets(10_000_000, 2, 16)

	// durationBuckets: 1 min → ~8.5 h, doubling each step (10 buckets).
	// Covers both source-media content duration and wall-clock transcode
	// runtime (which span roughly the same range depending on hardware
	// acceleration and source bitrate).
	durationBuckets = tally.MustMakeExponentialDurationBuckets(time.Minute, 2, 10)
)

// Metric names. The tally→Prometheus naming scope appends _total to counters
// and _seconds to timers; histograms are emitted verbatim.
const (
	metricInvalidFiles        = "media_workflow_invalid_files"
	metricArtworkFetchSkipped = "media_workflow_artwork_fetch_skipped"
	// metricImportSkippedNotInLibrary counts files whose library import was
	// skipped because the media item is no longer in the arr library.
	metricImportSkippedNotInLibrary = "media_workflow_import_skipped_not_in_library"
	metricMetricsErrors             = "media_workflow_metrics_errors"
	metricAudioTrackCount           = "media_workflow_audio_track_count"
	metricSubtitleTrackCount        = "media_workflow_subtitle_track_count"
	metricSourceDurationSeconds     = "media_workflow_source_duration_seconds"
	metricSourceFileSizeBytes       = "media_workflow_source_file_size_bytes"
	metricDestFileSizeBytes         = "media_workflow_destination_file_size_bytes"
	metricTranscodeDurationSecs     = "media_workflow_transcode_duration_seconds"
)

// baseTags returns the tag map carried by every media-workflow metric
// emission, keyed on the inputs available before any activity has run.
func baseTags(input mediatypes.MediaInput) map[string]string {
	return map[string]string{
		"media_type":   string(input.MediaType),
		"mapping_name": input.MappingName,
	}
}

// transcodeTags returns the tag map carried by metrics emitted after the
// transcode step has completed. Includes probe- and transcode-derived fields
// in addition to the base tags. High-cardinality tags are merged in when
// non-nil; the keys must match the full registered set (see
// resolveHighCardinalityLabels) so the tally→Prometheus reporter does not
// see a varying tag-key set across emissions for the same metric name.
func transcodeTags(input mediatypes.MediaInput, probe ProbeOutput, transcode TranscodeOutput, hcTags map[string]string) map[string]string {
	tags := baseTags(input)
	tags["source_codec"] = probe.VideoCodec
	tags["destination_codec"] = transcode.DestCodec
	tags["source_container"] = probe.Format
	tags["destination_container"] = transcode.DestContainer
	tags["hardware_accelerated"] = strconv.FormatBool(transcode.HardwareAccelerated)
	tags["crop_applied"] = strconv.FormatBool(transcode.CropApplied)

	for k, v := range hcTags {
		tags[k] = v
	}

	return tags
}
