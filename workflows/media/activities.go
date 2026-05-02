package media

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/steps"
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
	recorder      *Recorder
}

// NewActivities constructs an Activities ready for registration. Defaults are
// applied to cfg.DetectCropTimeout and cfg.TranscodeTimeout when zero, and a
// metrics Recorder is built from cfg.MetricsRegisterer (with a private
// throwaway registry as a fallback if registration against the supplied
// registerer fails).
func NewActivities(cfg MediaWorkflowConfig, radarrClient, sonarrClient medialib.ArrLibrary, webhookClient *webhook.Client) (*Activities, error) {
	if cfg.DetectCropTimeout == 0 {
		cfg.DetectCropTimeout = DefaultDetectCropTimeout
	}

	if cfg.TranscodeTimeout == 0 {
		cfg.TranscodeTimeout = DefaultTranscodeTimeout
	}

	registerer := cfg.MetricsRegisterer
	if registerer == nil {
		registerer = prometheus.NewRegistry()
	}

	recorder, err := NewRecorder(registerer, cfg.HighCardinalityLabels)
	if err != nil {
		// Registration errors are non-fatal: log for observability and fall
		// back to a private registry so the workflow can still run without
		// exposing metrics.
		slog.Warn("media: failed to register metrics collectors, falling back to private registry", "error", err)

		var noopErr error

		recorder, noopErr = NewRecorder(prometheus.NewRegistry(), false)
		if noopErr != nil {
			return nil, fmt.Errorf("create fallback metrics recorder: %w", noopErr)
		}
	}

	return &Activities{
		cfg:           cfg,
		radarrClient:  radarrClient,
		sonarrClient:  sonarrClient,
		webhookClient: webhookClient,
		recorder:      recorder,
	}, nil
}

// Register attaches the workflow function and the eight activities to the
// given Temporal worker.
func (a *Activities) Register(w worker.Worker) {
	w.RegisterWorkflowWithOptions(a.MediaWorkflow, workflow.RegisterOptions{Name: MediaWorkflowName})
	w.RegisterActivityWithOptions(a.Probe, activity.RegisterOptions{Name: ProbeActivityName})
	w.RegisterActivityWithOptions(a.DetectCrop, activity.RegisterOptions{Name: DetectCropActivityName})
	w.RegisterActivityWithOptions(a.Transcode, activity.RegisterOptions{Name: TranscodeActivityName})
	w.RegisterActivityWithOptions(a.Notify, activity.RegisterOptions{Name: NotifyActivityName})
	w.RegisterActivityWithOptions(a.Cleanup, activity.RegisterOptions{Name: CleanupActivityName})
	w.RegisterActivityWithOptions(a.RecordRunMetrics, activity.RegisterOptions{Name: RecordRunMetricsActivityName})
	w.RegisterActivityWithOptions(a.RecordInvalid, activity.RegisterOptions{Name: RecordInvalidActivityName})
	w.RegisterActivityWithOptions(a.NotifyFailure, activity.RegisterOptions{Name: NotifyFailureActivityName})
}

// Probe is the Temporal activity that wraps steps.RunProbe. It records the
// step start time on the returned ProbeOutput so downstream metrics can compute
// the full probe→cleanup wall-clock duration.
func (a *Activities) Probe(ctx context.Context, input MediaInput) (steps.ProbeOutput, error) {
	start := time.Now()

	out, err := steps.RunProbe(ctx, input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
	logStepResult(ctx, "probe", input.FilePath, start, err)

	if err != nil {
		return out, err
	}

	out.StartedAt = start

	return out, nil
}

// DetectCrop is the Temporal activity that wraps steps.RunDetectCrop. It
// returns a DetectCropOutput with a nil Crop when the cropdetect filter
// produced no usable crop region.
func (a *Activities) DetectCrop(ctx context.Context, input MediaInput, probe steps.ProbeOutput) (steps.DetectCropOutput, error) {
	start := time.Now()

	crop, err := steps.RunDetectCrop(ctx, input.FilePath, probe.VideoWidth, probe.VideoHeight, a.cfg.MinCropX, a.cfg.MinCropY)
	logStepResult(ctx, "detectcrop", input.FilePath, start, err)

	if err != nil {
		return steps.DetectCropOutput{}, err
	}

	return steps.DetectCropOutput{Crop: crop}, nil
}

// Transcode is the Temporal activity that wraps steps.RunTranscode. It also
// emits the artwork-fetch-skipped counter when the transcode succeeded but no
// poster image was attached.
func (a *Activities) Transcode(ctx context.Context, input MediaInput, probe steps.ProbeOutput, cropOut steps.DetectCropOutput) (steps.TranscodeOutput, error) {
	start := time.Now()

	library, err := getArrLibrary(input.MediaType, a.radarrClient, a.sonarrClient)
	if err != nil {
		wrappedErr := fmt.Errorf("get arr library for artwork: %w", err)
		logStepResult(ctx, "transcode", input.FilePath, start, wrappedErr)

		return steps.TranscodeOutput{}, wrappedErr
	}

	outputPath := filepath.Clean(strings.TrimSpace(input.OutputPath))
	if outputPath == "" || outputPath == "." {
		emptyErr := fmt.Errorf("output_path is required")
		logStepResult(ctx, "transcode", input.FilePath, start, emptyErr)

		return steps.TranscodeOutput{}, emptyErr
	}

	out, err := steps.RunTranscode(ctx, input.FilePath, probe, cropOut.Crop, outputPath, input.WatchRoot, a.cfg.HardwareDevicePath, a.cfg.H265CRF, a.cfg.ProgressLogInterval, library)
	if err == nil && out.ArtworkFetchSkipped {
		a.recorder.RecordArtworkFetchSkipped()
	}

	logStepResult(ctx, "transcode", input.FilePath, start, err)

	return out, err
}

// Notify issues the library import (Sonarr/Radarr scan command). The import is
// idempotent in practice — re-issuing the same scan for an already-imported
// file is a no-op — so the workflow retries this activity on transient
// failures. Pure-data errors (unknown media type, output_remote_path outside
// the output tree) are returned as non-retryable so Temporal does not burn the
// retry budget on inputs that cannot recover.
func (a *Activities) Notify(ctx context.Context, input MediaInput, transcode steps.TranscodeOutput) error {
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
// by Probe and Cleanup is a near-no-op, but the sentinel branch is still
// exercised when PreserveSource is set.
func (a *Activities) Cleanup(ctx context.Context, input MediaInput) error {
	start := time.Now()

	var err error
	if input.PreserveSource {
		err = steps.WriteSentinel(input.FilePath)
	} else {
		err = steps.RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
	}

	logStepResult(ctx, "cleanup", input.FilePath, start, err)

	return err
}

// RecordRunMetrics records per-run histograms and (when high-cardinality
// labels are enabled) attaches Sonarr/Radarr metadata via GetInfo. Histogram
// emission is not idempotent — every Record() call adds a fresh sample — so
// the workflow invokes this activity with MaximumAttempts: 1 and ignores the
// returned error. A metrics issue must not fail an otherwise-successful run
// after the file has already been imported and cleaned up.
func (a *Activities) RecordRunMetrics(ctx context.Context, input MediaInput, probe steps.ProbeOutput, transcode steps.TranscodeOutput) error {
	start := time.Now()

	var mediaInfo medialib.MediaInfo

	if a.cfg.HighCardinalityLabels {
		if lib, libErr := getArrLibrary(input.MediaType, a.radarrClient, a.sonarrClient); libErr == nil {
			info, infoErr := lib.GetInfo(ctx, input.FilePath)
			if infoErr != nil {
				// GetInfo failure is best-effort: log + count it but do not
				// fail the activity. Metrics are still recorded without the
				// high-cardinality labels.
				a.recorder.RecordMetricsError(fmt.Errorf("GetInfo: %w", infoErr))
			} else {
				mediaInfo = info
			}
		}
	}

	totalElapsed := time.Since(probe.StartedAt)
	a.recorder.RecordRun(input, probe, transcode, mediaInfo, transcode.HardwareAccelerated, totalElapsed)

	logStepResult(ctx, "record_run_metrics", input.FilePath, start, nil)

	return nil
}

// RecordInvalid increments the invalid-files counter for files that the probe
// activity determined were not valid media. Counter increment is not
// idempotent so the workflow invokes this activity with MaximumAttempts: 1
// and ignores the returned error.
func (a *Activities) RecordInvalid(ctx context.Context, input MediaInput) error {
	start := time.Now()

	a.recorder.RecordInvalidFile(input.MediaType, input.MappingName)

	logStepResult(ctx, "record_invalid", input.FilePath, start, nil)

	return nil
}

// NotifyFailure sends the configured failure webhook for a workflow that
// returned an error. Invoked from the workflow's defer block on any non-nil
// return; the failed step name and error message are sourced from the
// stepError that the workflow body returned.
func (a *Activities) NotifyFailure(ctx context.Context, input MediaInput, failedStep, failureMsg string) error {
	start := time.Now()

	stepErrors := map[string]string{}
	if failedStep != "" {
		stepErrors[failedStep] = failureMsg
	}

	err := steps.NotifyWorkflowFailure(ctx, stepErrors, MediaWorkflowName, input.FilePath, a.webhookClient)
	logStepResult(ctx, "notify_failure", input.FilePath, start, err)

	return err
}

func logStepResult(ctx context.Context, stepName, filePath string, start time.Time, err error) {
	if err != nil {
		slog.ErrorContext(ctx, "step failed", slog.String("step", stepName), slog.String("file", filePath), slog.Any("error", err))
		return
	}

	slog.InfoContext(ctx, "step complete", slog.String("step", stepName), slog.String("file", filePath), slog.Duration("elapsed", time.Since(start)))
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
