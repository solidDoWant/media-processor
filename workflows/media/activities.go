package media

import (
	"context"
	"errors"
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
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
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
	cfg                MediaWorkflowConfig
	hardwareDevicePath string
	radarrClient       medialib.ArrLibrary
	sonarrClient       medialib.ArrLibrary
	webhookClient      *webhook.Client
}

// ActivitiesOption configures optional fields on the Activities returned by
// NewActivities. Required dependencies (cfg, arr clients, webhook) stay as
// positional args; per-worker values that are not always relevant (e.g. the
// hardware device path, which is only meaningful on a transcode-enabled worker)
// flow through here so test callers don't have to thread them.
type ActivitiesOption func(*Activities)

// WithHardwareDevicePath injects the device path used by the transcode
// activity for hardware-accelerated encoding. The empty string (the default)
// is forwarded to libav's CreateHardwareDeviceContext as "auto-select"; when
// no hardware backend is available the encoder layer falls back to libx265
// per the existing per-profile fallback in pkg/ffmpeg/stream_video.go. A
// non-empty path is used verbatim (validated as a character device at
// startup).
func WithHardwareDevicePath(path string) ActivitiesOption {
	return func(a *Activities) { a.hardwareDevicePath = path }
}

// NewActivities constructs an Activities ready for registration. Defaults are
// applied to cfg.TaskQueuePrefix, cfg.DetectCropTimeout, cfg.TranscodeTimeout,
// and the four cfg.Notify* retry-policy fields when zero.
func NewActivities(cfg MediaWorkflowConfig, radarrClient, sonarrClient medialib.ArrLibrary, webhookClient *webhook.Client, opts ...ActivitiesOption) (*Activities, error) {
	if cfg.TaskQueuePrefix == "" {
		cfg.TaskQueuePrefix = DefaultTaskQueuePrefix
	}

	if cfg.DetectCropTimeout == 0 {
		cfg.DetectCropTimeout = DefaultDetectCropTimeout
	}

	if cfg.TranscodeTimeout == 0 {
		cfg.TranscodeTimeout = DefaultTranscodeTimeout
	}

	if cfg.NotifyInitialInterval == 0 {
		cfg.NotifyInitialInterval = DefaultNotifyInitialInterval
	}

	if cfg.NotifyBackoffCoefficient == 0 {
		cfg.NotifyBackoffCoefficient = DefaultNotifyBackoffCoefficient
	}

	if cfg.NotifyMaximumInterval == 0 {
		cfg.NotifyMaximumInterval = DefaultNotifyMaximumInterval
	}

	if cfg.NotifyMaximumAttempts == 0 {
		cfg.NotifyMaximumAttempts = DefaultNotifyMaximumAttempts
	}

	a := &Activities{
		cfg:           cfg,
		radarrClient:  radarrClient,
		sonarrClient:  sonarrClient,
		webhookClient: webhookClient,
	}

	for _, opt := range opts {
		opt(a)
	}

	return a, nil
}

// Registrar is the subset of registration methods that both worker.Worker and
// *testsuite.TestWorkflowEnvironment expose. Accepting this interface lets the
// production worker and the workflow test environment share registration
// helpers, so adding or renaming an activity only requires editing the maps
// below.
type Registrar interface {
	RegisterWorkflowWithOptions(w any, options workflow.RegisterOptions)
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}

// activityRegistration pairs a Temporal-registered activity name with the
// method that implements it.
type activityRegistration struct {
	name string
	fn   any
}

// activityRegistrations returns the kebab-case-token → (name, fn) map used
// to register exactly one activity per Temporal Worker. Constructed per call
// because the function values close over the receiver.
func (a *Activities) activityRegistrations() map[string]activityRegistration {
	return map[string]activityRegistration{
		ProbeActivityToken:         {ProbeActivityName, a.Probe},
		DetectCropActivityToken:    {DetectCropActivityName, a.DetectCrop},
		TranscodeActivityToken:     {TranscodeActivityName, a.Transcode},
		NotifyActivityToken:        {NotifyActivityName, a.Notify},
		CleanupActivityToken:       {CleanupActivityName, a.Cleanup},
		NotifyFailureActivityToken: {NotifyFailureActivityName, a.NotifyFailure},
	}
}

// RegisterWorkflow attaches only the media workflow function to r. Used by
// the worker pod's workflow-task-queue Worker, which polls the prefix-only
// queue and runs no activities.
func (a *Activities) RegisterWorkflow(r Registrar) {
	r.RegisterWorkflowWithOptions(a.MediaWorkflow, workflow.RegisterOptions{Name: MediaWorkflowName})
}

// RegisterActivity attaches the activity identified by its kebab-case token
// (e.g. "transcode", "detect-crop") to r. Returns an error if the token is
// not a known activity name.
func (a *Activities) RegisterActivity(r Registrar, token string) error {
	entry, ok := a.activityRegistrations()[token]
	if !ok {
		return fmt.Errorf("unknown activity token %q", token)
	}

	r.RegisterActivityWithOptions(entry.fn, activity.RegisterOptions{Name: entry.name})

	return nil
}

// Register attaches the workflow function and every activity to r. Used by
// the workflow test environment, which runs everything on a single in-memory
// task queue.
func (a *Activities) Register(r Registrar) {
	a.RegisterWorkflow(r)

	for _, token := range KnownActivities {
		// Iterating KnownActivities directly cannot produce an unknown token,
		// so the error is impossible here.
		if err := a.RegisterActivity(r, token); err != nil {
			panic(err)
		}
	}
}

// Probe is the Temporal activity that wraps RunProbe. Per-run metrics
// are emitted inline: source-duration histogram + audio/subtitle gauges for
// valid media, or the invalid-files counter when the file fails to probe as
// media.
func (a *Activities) Probe(ctx context.Context, input MediaInput) (ProbeOutput, error) {
	start := time.Now()

	out, err := RunProbe(ctx, input.FilePath, input.WatchRoot, input.RetainEmptyDirectories, input.PreserveSource)
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

	heartbeat := func(p ffmpeg.Progress) {
		activity.RecordHeartbeat(ctx, p)
	}

	out, err := RunTranscode(ctx, TranscodeRequest{
		FilePath:            input.FilePath,
		Probe:               probe,
		CropParams:          cropOut.Crop,
		OutputDir:           outputPath,
		WatcherRoot:         input.WatchRoot,
		HardwareDevicePath:  a.hardwareDevicePath,
		H265CRF:             a.cfg.H265CRF,
		ProgressLogInterval: a.cfg.ProgressLogInterval,
		Heartbeat:           heartbeat,
		Library:             library,
	})
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
// through the standard MetricsHandler interface. The transcode-duration
// histogram is suppressed when no encoding ran (existing output reused), so
// the metric reflects only real transcode work and is not skewed by reuses.
func emitTranscodeMetrics(ctx context.Context, input MediaInput, probe ProbeOutput, transcode TranscodeOutput, hcTags map[string]string) {
	handler := activity.GetMetricsHandler(ctx)
	tags := transcodeTags(input, probe, transcode, hcTags)

	scope := scopedHistograms(handler, tags)
	scope.Histogram(metricSourceFileSizeBytes, fileSizeBuckets).RecordValue(float64(transcode.SourceFileSizeBytes))
	scope.Histogram(metricDestFileSizeBytes, fileSizeBuckets).RecordValue(float64(transcode.DestFileSizeBytes))

	if transcode.TranscodeDurationSeconds > 0 {
		scope.Histogram(metricTranscodeDurationSecs, durationBuckets).
			RecordDuration(time.Duration(transcode.TranscodeDurationSeconds * float64(time.Second)))
	}

	if transcode.ArtworkFetchSkipped {
		handler.Counter(metricArtworkFetchSkipped).Inc(1)
	}
}

// NotifyOutput is the result of the Notify activity. ImportSkipped is true when
// the library import was deliberately skipped because the import can never
// succeed: either the media item is no longer present in the arr library (the
// movie/series was removed or is no longer monitored), or the library already
// holds an equal or better file so the release was rejected as "not an upgrade".
// The workflow forwards it to Cleanup so the orphaned transcoded output file —
// which a successful arr import would normally consume by moving it into the
// library — is removed.
type NotifyOutput struct {
	ImportSkipped bool `json:"import_skipped,omitempty"`
}

// Notify issues the library import (Sonarr/Radarr scan command). The import is
// idempotent in practice — re-issuing the same scan for an already-imported
// file is a no-op — so the workflow retries this activity on transient
// failures. Pure-data errors (unknown media type, output_remote_path outside
// the output tree) are returned as non-retryable so Temporal does not burn the
// retry budget on inputs that cannot recover.
//
// When the arr service reports the media item is no longer in its library, or
// rejects the release because its existing file is already an equal or better
// version, the import can never succeed. That is an expected, benign outcome
// rather than a failure: Notify records it, returns NotifyOutput{ImportSkipped:
// true} with a nil error so the workflow proceeds to Cleanup (which removes the
// orphaned output file), and avoids both the retry budget and the failure webhook.
func (a *Activities) Notify(ctx context.Context, input MediaInput, transcode TranscodeOutput) (NotifyOutput, error) {
	start := time.Now()

	library, err := getArrLibrary(input.MediaType, a.radarrClient, a.sonarrClient)
	if err != nil {
		nonRetryable := temporal.NewNonRetryableApplicationError(err.Error(), errTypeNonRetryable, err)
		logStepResult(ctx, "notify", input.FilePath, start, nonRetryable)

		return NotifyOutput{}, nonRetryable
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

			return NotifyOutput{}, nonRetryable
		}

		importPath = filepath.Join(remotePath, rel)
	}

	// Use the size recorded at transcode time rather than re-statting the file,
	// which may no longer be present if the arr service has already moved it
	// during a directory-level import (e.g. a series pack).
	if err := library.ImportByFilePath(ctx, importPath, transcode.DestFileSizeBytes); err != nil {
		// Benign skip, not a failure (see the doc above): return success so
		// Temporal does not retry and the failure webhook does not fire.
		if errors.Is(err, medialib.ErrNotFound) {
			activity.GetLogger(ctx).Info("media item no longer in library; skipping import",
				"file", input.FilePath, "import_path", importPath)
			activity.GetMetricsHandler(ctx).WithTags(baseTags(input)).Counter(metricImportSkippedNotInLibrary).Inc(1)
			logStepResult(ctx, "notify", input.FilePath, start, nil)

			return NotifyOutput{ImportSkipped: true}, nil
		}

		// The arr service rejected the import because its existing file is already
		// an equal or better version. Like the not-in-library case, the import can
		// never succeed, so skip it rather than retrying or firing the webhook.
		if errors.Is(err, medialib.ErrNotUpgrade) {
			activity.GetLogger(ctx).Info("library already has an equal or better file; skipping import",
				"file", input.FilePath, "import_path", importPath)
			activity.GetMetricsHandler(ctx).WithTags(baseTags(input)).Counter(metricImportSkippedNotUpgrade).Inc(1)
			logStepResult(ctx, "notify", input.FilePath, start, nil)

			return NotifyOutput{ImportSkipped: true}, nil
		}

		wrappedErr := fmt.Errorf("notify library: %w", err)
		logStepResult(ctx, "notify", input.FilePath, start, wrappedErr)

		return NotifyOutput{}, wrappedErr
	}

	logStepResult(ctx, "notify", input.FilePath, start, nil)

	return NotifyOutput{}, nil
}

// Cleanup deletes the source file or writes the .done sentinel, then prunes
// the mirrored subdirectory under OutputPath if Notify has drained the
// transcoded file. RunCleanup tolerates ErrNotExist so retrying after a
// partial cleanup is safe; WriteSentinel re-writes the same zero-byte file.
// Output-side pruning is best-effort: it only removes truly-empty
// directories, so a copied-but-not-moved transcode is left in place.
// Shared between the valid and invalid paths — in the invalid path the
// source has already been removed by Probe (RunCleanup is a near-no-op),
// transcode.DestFilePath is empty, and output pruning is skipped.
// When notify.ImportSkipped is set (the media item is no longer in the arr
// library, or the library already holds an equal or better file), the arr
// service will not move the transcoded file into its library, so the orphaned
// output file is removed here before pruning.
func (a *Activities) Cleanup(ctx context.Context, input MediaInput, transcode TranscodeOutput, notify NotifyOutput) error {
	start := time.Now()

	if notify.ImportSkipped {
		if err := RemoveOutputFile(transcode.DestFilePath); err != nil {
			logStepResult(ctx, "cleanup", input.FilePath, start, err)

			return err
		}
	}

	var err error
	if input.PreserveSource {
		err = WriteSentinel(input.FilePath)
	} else {
		err = RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
	}

	if err == nil && !input.RetainEmptyDirectories {
		PruneOutputDirs(transcode.DestFilePath, input.OutputPath)
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
