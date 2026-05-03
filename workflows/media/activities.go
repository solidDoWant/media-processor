package media

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/uber-go/tally/v4"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// errTypeNonRetryable tags ApplicationErrors raised for pure-data problems
// (unknown media type, malformed remote-path config) so Temporal will not
// burn the activity's retry budget on inputs that cannot recover.
const errTypeNonRetryable = "MediaInputError"

// Activities holds the dependencies needed by the activity methods. A single
// instance is registered with the worker; all activities share its fields.
type Activities struct {
	cfg           MediaWorkflowConfig
	radarrClient  medialib.ArrLibrary
	sonarrClient  medialib.ArrLibrary
	webhookClient *webhook.Client
}

// NewActivities constructs an Activities ready for registration. Defaults are
// applied to cfg.DetectCropTimeout and cfg.TranscodeTimeout when zero.
func NewActivities(cfg MediaWorkflowConfig, radarrClient, sonarrClient medialib.ArrLibrary, webhookClient *webhook.Client) (*Activities, error) {
	if cfg.DetectCropTimeout == 0 {
		cfg.DetectCropTimeout = DefaultDetectCropTimeout
	}

	if cfg.TranscodeTimeout == 0 {
		cfg.TranscodeTimeout = DefaultTranscodeTimeout
	}

	return &Activities{
		cfg:           cfg,
		radarrClient:  radarrClient,
		sonarrClient:  sonarrClient,
		webhookClient: webhookClient,
	}, nil
}

// Register attaches the workflow function and the six activities to the given
// Temporal worker.
func (a *Activities) Register(w worker.Worker) {
	w.RegisterWorkflowWithOptions(a.MediaWorkflow, workflow.RegisterOptions{Name: MediaWorkflowName})
	w.RegisterActivityWithOptions(a.Probe, activity.RegisterOptions{Name: ProbeActivityName})
	w.RegisterActivityWithOptions(a.DetectCrop, activity.RegisterOptions{Name: DetectCropActivityName})
	w.RegisterActivityWithOptions(a.Transcode, activity.RegisterOptions{Name: TranscodeActivityName})
	w.RegisterActivityWithOptions(a.Notify, activity.RegisterOptions{Name: NotifyActivityName})
	w.RegisterActivityWithOptions(a.Cleanup, activity.RegisterOptions{Name: CleanupActivityName})
	w.RegisterActivityWithOptions(a.NotifyFailure, activity.RegisterOptions{Name: NotifyFailureActivityName})
}

// Probe is the Temporal activity that wraps RunProbe. Per-run metrics
// are emitted inline: source-duration histogram + audio/subtitle gauges for
// valid media, or the invalid-files counter when the file fails to probe as
// media.
func (a *Activities) Probe(ctx context.Context, input MediaInput) (ProbeOutput, error) {
	start := time.Now()

	out, err := RunProbe(ctx, input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
	logStepResult(ctx, "probe", input.FilePath, start, err)

	if err != nil {
		return out, err
	}

	emitProbeMetrics(ctx, input, out)

	return out, nil
}

// emitProbeMetrics reports the per-probe observations to the SDK metrics
// pipeline. Histogram emission uses the underlying tally scope obtained via
// contribtally.ScopeFromHandler so source-duration carries an explicit bucket
// set; counters and gauges go through the standard MetricsHandler interface.
func emitProbeMetrics(ctx context.Context, input MediaInput, probe ProbeOutput) {
	handler := activity.GetMetricsHandler(ctx)
	tags := baseTags(input)

	if !probe.IsValidMedia {
		handler.WithTags(tags).Counter(metricInvalidFiles).Inc(1)
		return
	}

	tagged := handler.WithTags(tags)
	tagged.Gauge(metricAudioTrackCount).Update(float64(len(probe.AudioStreams)))
	tagged.Gauge(metricSubtitleTrackCount).Update(float64(len(probe.SubtitleStreams)))

	scopedHistograms(handler, tags).
		Histogram(metricSourceDurationSeconds, durationBuckets).
		RecordDuration(time.Duration(probe.DurationSeconds * float64(time.Second)))
}

// scopedHistograms returns the tally scope that backs handler, tagged with
// tags. Activities are not replayed, so no IsReplaying guard is needed (unlike
// workflow code).
func scopedHistograms(handler client.MetricsHandler, tags map[string]string) tally.Scope {
	return contribtally.ScopeFromHandler(handler).Tagged(tags)
}

// DetectCrop is the Temporal activity that wraps RunDetectCrop. It
// returns a DetectCropOutput with a nil Crop when the cropdetect filter
// produced no usable crop region.
func (a *Activities) DetectCrop(ctx context.Context, input MediaInput, probe ProbeOutput) (DetectCropOutput, error) {
	start := time.Now()

	crop, err := RunDetectCrop(ctx, input.FilePath, probe.VideoWidth, probe.VideoHeight, a.cfg.MinCropX, a.cfg.MinCropY)
	logStepResult(ctx, "detectcrop", input.FilePath, start, err)

	if err != nil {
		return DetectCropOutput{}, err
	}

	return DetectCropOutput{Crop: crop}, nil
}

// Transcode is the Temporal activity that wraps RunTranscode. After a
// successful transcode it emits the per-run file-size and transcode-duration
// histograms and the artwork-fetch-skipped counter when applicable. When
// high-cardinality labels are enabled it also looks up per-item metadata
// from the arr library to attach as tags. The SDK's
// temporal_activity_execution_latency_seconds also tracks transcode wall-clock,
// but only with worker-context tags; the application metric carries the media
// tags (codec, container, hardware_accelerated, crop_applied, …) that make
// runtime correlatable with source size, source duration, and codec choice.
func (a *Activities) Transcode(ctx context.Context, input MediaInput, probe ProbeOutput, cropOut DetectCropOutput) (TranscodeOutput, error) {
	start := time.Now()

	library, err := getArrLibrary(input.MediaType, a.radarrClient, a.sonarrClient)
	if err != nil {
		wrappedErr := fmt.Errorf("get arr library for artwork: %w", err)
		logStepResult(ctx, "transcode", input.FilePath, start, wrappedErr)

		return TranscodeOutput{}, wrappedErr
	}

	outputPath := filepath.Clean(strings.TrimSpace(input.OutputPath))
	if outputPath == "" || outputPath == "." {
		emptyErr := fmt.Errorf("output_path is required")
		logStepResult(ctx, "transcode", input.FilePath, start, emptyErr)

		return TranscodeOutput{}, emptyErr
	}

	out, err := RunTranscode(ctx, input.FilePath, probe, cropOut.Crop, outputPath, input.WatchRoot, a.cfg.HardwareDevicePath, a.cfg.H265CRF, a.cfg.ProgressLogInterval, library)
	logStepResult(ctx, "transcode", input.FilePath, start, err)

	if err != nil {
		return out, err
	}

	var hcTags map[string]string
	if a.cfg.HighCardinalityLabels {
		hcTags = a.resolveHighCardinalityLabels(ctx, input, library)
	}

	emitTranscodeMetrics(ctx, input, probe, out, hcTags)

	return out, nil
}

// resolveHighCardinalityLabels fetches per-item metadata from the arr library
// and returns it as a tag map (id, title, year, plus episode-specific fields
// for shows). The full key set is always returned: episode-specific keys
// carry empty strings for movies, and on lookup failure every key carries
// an empty string. Returning a stable key set is required because tally's
// prom reporter caches metrics on (name, sortedTagKeys); a varying tag-key
// set across emissions for the same metric name would trigger a Prometheus
// "same name, different label names" registration conflict and silently
// drop one variant. On failure the metrics-errors counter is incremented.
func (a *Activities) resolveHighCardinalityLabels(ctx context.Context, input MediaInput, library medialib.ArrLibrary) map[string]string {
	tags := map[string]string{
		"id":             "",
		"title":          "",
		"year":           "",
		"series_title":   "",
		"season_number":  "",
		"episode_number": "",
	}

	info, err := library.GetInfo(ctx, input.FilePath)
	if err != nil || info == nil {
		if err != nil {
			activity.GetLogger(ctx).Warn("media workflow metrics: GetInfo failed", "error", err)
		}

		activity.GetMetricsHandler(ctx).Counter(metricMetricsErrors).Inc(1)

		return tags
	}

	tags["id"] = strconv.FormatInt(info.GetID(), 10)
	tags["title"] = info.GetTitle()
	tags["year"] = strconv.Itoa(info.GetYear())

	if input.MediaType == medialib.ShowType {
		tags["series_title"] = info.GetSeriesTitle()
		tags["season_number"] = strconv.Itoa(info.GetSeasonNumber())
		tags["episode_number"] = strconv.Itoa(info.GetEpisodeNumber())
	}

	return tags
}

// emitTranscodeMetrics reports the per-transcode observations to the SDK
// metrics pipeline. File-size histograms use explicit buckets via the tally
// scope obtained from the SDK handler; the artwork-fetch-skipped counter goes
// through the standard MetricsHandler interface.
func emitTranscodeMetrics(ctx context.Context, input MediaInput, probe ProbeOutput, transcode TranscodeOutput, hcTags map[string]string) {
	handler := activity.GetMetricsHandler(ctx)
	tags := transcodeTags(input, probe, transcode, hcTags)

	scope := scopedHistograms(handler, tags)
	scope.Histogram(metricSourceFileSizeBytes, fileSizeBuckets).RecordValue(float64(transcode.SourceFileSizeBytes))
	scope.Histogram(metricDestFileSizeBytes, fileSizeBuckets).RecordValue(float64(transcode.DestFileSizeBytes))
	scope.Histogram(metricTranscodeDurationSecs, durationBuckets).
		RecordDuration(time.Duration(transcode.TranscodeDurationSeconds * float64(time.Second)))

	if transcode.ArtworkFetchSkipped {
		handler.Counter(metricArtworkFetchSkipped).Inc(1)
	}
}

// Notify issues the library import (Sonarr/Radarr scan command). The import is
// idempotent in practice — re-issuing the same scan for an already-imported
// file is a no-op — so the workflow retries this activity on transient
// failures. Pure-data errors (unknown media type, output_remote_path outside
// the output tree) are returned as non-retryable so Temporal does not burn the
// retry budget on inputs that cannot recover.
func (a *Activities) Notify(ctx context.Context, input MediaInput, transcode TranscodeOutput) error {
	start := time.Now()

	library, err := getArrLibrary(input.MediaType, a.radarrClient, a.sonarrClient)
	if err != nil {
		nonRetryable := temporal.NewNonRetryableApplicationError(err.Error(), errTypeNonRetryable, err)
		logStepResult(ctx, "notify", input.FilePath, start, nonRetryable)

		return nonRetryable
	}

	importPath := transcode.DestFilePath

	if remotePath := strings.TrimSpace(input.OutputRemotePath); remotePath != "" {
		outputPath := filepath.Clean(strings.TrimSpace(input.OutputPath))

		rel, relErr := filepath.Rel(outputPath, importPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			nonRetryable := temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("output file %q is not under output_path %q; cannot apply output_remote_path substitution", importPath, input.OutputPath),
				errTypeNonRetryable,
				nil,
			)
			logStepResult(ctx, "notify", input.FilePath, start, nonRetryable)

			return nonRetryable
		}

		importPath = filepath.Join(remotePath, rel)
	}

	if err := library.ImportByFilePath(ctx, importPath); err != nil {
		wrappedErr := fmt.Errorf("notify library: %w", err)
		logStepResult(ctx, "notify", input.FilePath, start, wrappedErr)

		return wrappedErr
	}

	logStepResult(ctx, "notify", input.FilePath, start, nil)

	return nil
}

// Cleanup deletes the source file or writes the .done sentinel. RunCleanup
// tolerates ErrNotExist so retrying after a partial cleanup is safe;
// WriteSentinel re-writes the same zero-byte file. Shared between the valid
// and invalid paths — in the invalid path the source has already been removed
// by Probe and Cleanup is a near-no-op for the file, but the sentinel branch
// is still exercised when PreserveSource is set.
func (a *Activities) Cleanup(ctx context.Context, input MediaInput) error {
	start := time.Now()

	var err error
	if input.PreserveSource {
		err = WriteSentinel(input.FilePath)
	} else {
		err = RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
	}

	logStepResult(ctx, "cleanup", input.FilePath, start, err)

	return err
}

// NotifyFailure sends the configured failure webhook for a workflow that
// returned an error. Invoked from the workflow's defer block on any non-nil
// return; the failed step name comes from temporal.ActivityError.ActivityType()
// and the message from the wrapped activity error.
func (a *Activities) NotifyFailure(ctx context.Context, input MediaInput, failedStep, failureMsg string) error {
	start := time.Now()

	err := NotifyWorkflowFailure(ctx, failedStep, failureMsg, MediaWorkflowName, input.FilePath, a.webhookClient)
	logStepResult(ctx, "notify_failure", input.FilePath, start, err)

	return err
}

func logStepResult(ctx context.Context, stepName, filePath string, start time.Time, err error) {
	log := activity.GetLogger(ctx)
	if err != nil {
		log.Error("step failed", "step", stepName, "file", filePath, "error", err)
		return
	}

	log.Info("step complete", "step", stepName, "file", filePath, "elapsed", time.Since(start))
}

// getArrLibrary returns the LibraryClient corresponding to mediaType, using
// radarrClient for movies and sonarrClient for TV episodes.
func getArrLibrary(mediaType medialib.MediaType, radarrClient, sonarrClient medialib.ArrLibrary) (medialib.ArrLibrary, error) {
	switch mediaType {
	case medialib.MovieType:
		return radarrClient, nil
	case medialib.ShowType:
		return sonarrClient, nil
	default:
		return nil, fmt.Errorf("unknown media type %q", mediaType)
	}
}
