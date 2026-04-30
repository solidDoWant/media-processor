// Package media provides the Temporal workflow definition for processing media files
// (movies and TV episodes) using a single parameterised workflow.
package media

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// MediaWorkflowName is the registered Temporal workflow name. Re-exported from the
// types package for callers that import workflows/media directly.
const MediaWorkflowName = mediatypes.MediaWorkflowName

// Registered activity names. Workflows reference activities by these strings so
// that registration and invocation cannot drift from one another.
const (
	ProbeActivityName      = "Probe"
	DetectCropActivityName = "DetectCrop"
	TranscodeActivityName  = "Transcode"
	FinalizeActivityName   = "Finalize"
)

const (
	// DefaultDetectCropTimeout is the default Temporal StartToCloseTimeout for the
	// detectcrop activity, used when MediaWorkflowConfig.DetectCropTimeout is zero.
	DefaultDetectCropTimeout = 30 * time.Minute
	// DefaultTranscodeTimeout is the default Temporal StartToCloseTimeout for the
	// transcode activity, used when MediaWorkflowConfig.TranscodeTimeout is zero.
	DefaultTranscodeTimeout = 4 * time.Hour

	// defaultProbeTimeout is the StartToCloseTimeout applied to the probe activity.
	defaultProbeTimeout = 5 * time.Minute
	// defaultFinalizeTimeout is the StartToCloseTimeout applied to the finalize
	// activity in all three modes (valid / invalid / failure).
	defaultFinalizeTimeout = 10 * time.Minute

	// defaultMaxAttempts is the RetryPolicy MaximumAttempts applied to probe,
	// detectcrop, and transcode: these activities are not retried because their
	// failure modes (corrupt input, missing crop region, ffmpeg crash) generally
	// will not recover on a retry.
	defaultMaxAttempts = 1
	// finalizeValidMaxAttempts is the RetryPolicy MaximumAttempts applied to
	// the valid-path Finalize invocation. Library import and source cleanup are
	// idempotent and benefit from retries when the arr service or filesystem is
	// transiently unavailable.
	finalizeValidMaxAttempts = 3
)

// MediaWorkflowConfig holds the configuration for the media processing workflow
// and its activities.
type MediaWorkflowConfig struct {
	// HardwareDevicePath is the device path passed to CreateHardwareDeviceContext
	// for hardware-accelerated transcoding. An empty string uses libav auto-select.
	HardwareDevicePath string
	// MeterProvider is the OTel MeterProvider used for per-run metrics. When nil,
	// a no-op provider is used and no metrics are emitted.
	MeterProvider otelmetric.MeterProvider
	// HighCardinalityLabels controls whether per-item labels (id, title, year, etc.)
	// are attached to metric observations. Corresponds to METRICS_HIGH_CARDINALITY_LABELS.
	HighCardinalityLabels bool
	// MinCropX is the minimum number of pixels that must be trimmed horizontally
	// for a crop to be applied. -1 disables the threshold (any crop is accepted).
	// 0 means no minimum (any detected crop is applied).
	MinCropX int
	// MinCropY is the minimum number of pixels that must be trimmed vertically
	// for a crop to be applied. -1 disables the threshold (any crop is accepted).
	// 0 means no minimum (any detected crop is applied).
	MinCropY int
	// DetectCropTimeout is the Temporal StartToCloseTimeout for the detectcrop
	// activity. When zero, NewActivities applies a default of 30 minutes.
	DetectCropTimeout time.Duration
	// TranscodeTimeout is the Temporal StartToCloseTimeout for the transcode
	// activity. When zero, NewActivities applies a default of 4 hours.
	TranscodeTimeout time.Duration
	// H265CRF is the constant-quality value passed to H.265 encoders. 0 means
	// use the encoder's built-in default.
	H265CRF int
	// ProgressLogInterval controls how often a progress log line is emitted
	// during transcoding. Zero disables progress logging.
	ProgressLogInterval time.Duration
}

// MediaInput is an alias for the shared input type so existing callers within
// this package do not need to be updated.
type MediaInput = mediatypes.MediaInput

// FinalizeMode discriminates the three branches of the Finalize activity.
// Folding three short side-effecting tasks (record_metrics, record_invalid,
// finalize/cleanup) plus the failure-webhook into one activity keeps the
// workflow at four registered activities total.
type FinalizeMode int

const (
	// FinalizeValid runs the post-transcode work for a successfully processed
	// file: library import, source cleanup or sentinel, and per-run metrics.
	FinalizeValid FinalizeMode = iota + 1
	// FinalizeInvalid runs the cleanup-and-metrics path for a file that the
	// probe activity determined was not valid media.
	FinalizeInvalid
	// FinalizeFailure runs from the workflow's defer block to send a single
	// aggregated failure notification when the workflow returns an error.
	FinalizeFailure
)

// FinalizeInput is the activity payload for Finalize. Only the fields relevant
// to the chosen Mode are read.
type FinalizeInput struct {
	Mode      FinalizeMode
	Input     MediaInput
	Probe     steps.ProbeOutput     // valid + invalid modes
	Transcode steps.TranscodeOutput // valid mode

	// FailureStep is the activity name where the workflow error originated.
	// Only set when Mode == FinalizeFailure.
	FailureStep string
	// FailureErr is the error message from the failed activity. Only set when
	// Mode == FinalizeFailure.
	FailureErr string
}

// Activities holds the dependencies needed by the four activity methods. A
// single instance is registered with the worker; all activities share its
// fields.
type Activities struct {
	cfg           MediaWorkflowConfig
	radarrClient  medialib.ArrLibrary
	sonarrClient  medialib.ArrLibrary
	webhookClient *webhook.Client
	recorder      *Recorder
}

// NewActivities constructs an Activities ready for registration. Defaults are
// applied to cfg.DetectCropTimeout and cfg.TranscodeTimeout when zero, and a
// metrics Recorder is built from cfg.MeterProvider (with a noop fallback if
// instrument registration fails).
func NewActivities(cfg MediaWorkflowConfig, radarrClient, sonarrClient medialib.ArrLibrary, webhookClient *webhook.Client) (*Activities, error) {
	if cfg.DetectCropTimeout == 0 {
		cfg.DetectCropTimeout = DefaultDetectCropTimeout
	}

	if cfg.TranscodeTimeout == 0 {
		cfg.TranscodeTimeout = DefaultTranscodeTimeout
	}

	meterProvider := cfg.MeterProvider
	if meterProvider == nil {
		meterProvider = noop.NewMeterProvider()
	}

	recorder, err := NewRecorder(meterProvider, cfg.HighCardinalityLabels)
	if err != nil {
		// Instrument registration errors are non-fatal: log for observability and
		// fall back to a noop recorder so the workflow can still run without metrics.
		slog.Warn("media: failed to create metrics recorder, falling back to noop", "error", err)

		var noopErr error

		recorder, noopErr = NewRecorder(noop.NewMeterProvider(), false)
		if noopErr != nil {
			return nil, fmt.Errorf("create noop metrics recorder: %w", noopErr)
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

// Register attaches the workflow function and the four activities to the given
// Temporal worker.
func (a *Activities) Register(w worker.Worker) {
	w.RegisterWorkflowWithOptions(a.MediaWorkflow, workflow.RegisterOptions{Name: MediaWorkflowName})
	w.RegisterActivityWithOptions(a.Probe, activity.RegisterOptions{Name: ProbeActivityName})
	w.RegisterActivityWithOptions(a.DetectCrop, activity.RegisterOptions{Name: DetectCropActivityName})
	w.RegisterActivityWithOptions(a.Transcode, activity.RegisterOptions{Name: TranscodeActivityName})
	w.RegisterActivityWithOptions(a.Finalize, activity.RegisterOptions{Name: FinalizeActivityName})
}

// MediaWorkflow processes one media file: probe, optional detectcrop and
// transcode, finalize. A defer block sends a failure-webhook notification when
// the workflow exits with an error.
func (a *Activities) MediaWorkflow(ctx workflow.Context, input MediaInput) (err error) {
	log := workflow.GetLogger(ctx)
	log.Info("processing file", "file", input.FilePath)

	// failedStep names the activity whose error caused the workflow to return.
	// The defer block reads it to label the failure-webhook payload.
	failedStep := ""

	defer func() {
		if err == nil {
			return
		}

		// The workflow's context is cancelled when the workflow returns an
		// error, so further activities must be scheduled on a disconnected
		// context to actually run.
		disconnected, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()

		failureCtx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
			StartToCloseTimeout: defaultFinalizeTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
		})

		notifyErr := workflow.ExecuteActivity(failureCtx, FinalizeActivityName, FinalizeInput{
			Mode:        FinalizeFailure,
			Input:       input,
			FailureStep: failedStep,
			FailureErr:  err.Error(),
		}).Get(failureCtx, nil)
		if notifyErr != nil {
			log.Error("failure-webhook activity failed", "error", notifyErr.Error())
		}
	}()

	probeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultProbeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var probe steps.ProbeOutput
	if probeErr := workflow.ExecuteActivity(probeCtx, ProbeActivityName, input).Get(probeCtx, &probe); probeErr != nil {
		failedStep = "probe"
		return probeErr
	}

	if !probe.IsValidMedia {
		invalidCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: defaultFinalizeTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
		})

		if finErr := workflow.ExecuteActivity(invalidCtx, FinalizeActivityName, FinalizeInput{
			Mode:  FinalizeInvalid,
			Input: input,
			Probe: probe,
		}).Get(invalidCtx, nil); finErr != nil {
			failedStep = "finalize"
			return finErr
		}

		return nil
	}

	cropCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: a.cfg.DetectCropTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var crop steps.DetectCropOutput
	if cropErr := workflow.ExecuteActivity(cropCtx, DetectCropActivityName, input, probe).Get(cropCtx, &crop); cropErr != nil {
		failedStep = "detectcrop"
		return cropErr
	}

	transcodeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: a.cfg.TranscodeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var transcode steps.TranscodeOutput
	if tErr := workflow.ExecuteActivity(transcodeCtx, TranscodeActivityName, input, probe, crop).Get(transcodeCtx, &transcode); tErr != nil {
		failedStep = "transcode"
		return tErr
	}

	finalizeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultFinalizeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: finalizeValidMaxAttempts},
	})

	if finErr := workflow.ExecuteActivity(finalizeCtx, FinalizeActivityName, FinalizeInput{
		Mode:      FinalizeValid,
		Input:     input,
		Probe:     probe,
		Transcode: transcode,
	}).Get(finalizeCtx, nil); finErr != nil {
		failedStep = "finalize"
		return finErr
	}

	return nil
}

// Probe is the Temporal activity that wraps steps.RunProbe. It records the
// step start time on the returned ProbeOutput so downstream metrics can compute
// the full probe→finalize wall-clock duration.
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
		a.recorder.RecordArtworkFetchSkipped(ctx)
	}

	logStepResult(ctx, "transcode", input.FilePath, start, err)

	return out, err
}

// Finalize is the Temporal activity that handles all post-probe / post-failure
// side effects. The Mode field of FinalizeInput selects between the valid,
// invalid, and failure-webhook code paths.
func (a *Activities) Finalize(ctx context.Context, fin FinalizeInput) error {
	start := time.Now()

	var err error

	switch fin.Mode {
	case FinalizeValid:
		err = a.finalizeValid(ctx, fin)
	case FinalizeInvalid:
		err = a.finalizeInvalid(ctx, fin)
	case FinalizeFailure:
		err = a.finalizeFailure(ctx, fin)
	default:
		err = fmt.Errorf("finalize: unknown mode %d", fin.Mode)
	}

	logStepResult(ctx, "finalize", fin.Input.FilePath, start, err)

	return err
}

// finalizeValid runs the success path for a transcoded file. Operations are
// ordered so that the non-idempotent metrics emission only runs after the
// idempotent steps (library import, cleanup) have completed: an activity retry
// triggered by an earlier failure cannot double-count metrics.
func (a *Activities) finalizeValid(ctx context.Context, fin FinalizeInput) error {
	input := fin.Input
	probe := fin.Probe
	transcode := fin.Transcode

	library, err := getArrLibrary(input.MediaType, a.radarrClient, a.sonarrClient)
	if err != nil {
		return err
	}

	importPath := transcode.DestFilePath

	if remotePath := strings.TrimSpace(input.OutputRemotePath); remotePath != "" {
		outputPath := filepath.Clean(strings.TrimSpace(input.OutputPath))

		rel, relErr := filepath.Rel(outputPath, importPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("output file %q is not under output_path %q; cannot apply output_remote_path substitution", importPath, input.OutputPath)
		}

		importPath = filepath.Join(remotePath, rel)
	}

	if err := library.ImportByFilePath(ctx, importPath); err != nil {
		return fmt.Errorf("notify library: %w", err)
	}

	if input.PreserveSource {
		if err := steps.WriteSentinel(input.FilePath); err != nil {
			return err
		}
	} else if err := steps.RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories); err != nil {
		return err
	}

	var mediaInfo medialib.MediaInfo

	if a.cfg.HighCardinalityLabels {
		if lib, libErr := getArrLibrary(input.MediaType, a.radarrClient, a.sonarrClient); libErr == nil {
			info, infoErr := lib.GetInfo(ctx, input.FilePath)
			if infoErr != nil {
				// GetInfo failure is best-effort: log + count it but do not
				// fail the activity. Metrics are still recorded without the
				// high-cardinality labels.
				a.recorder.RecordMetricsError(ctx, fmt.Errorf("GetInfo: %w", infoErr))
			} else {
				mediaInfo = info
			}
		}
	}

	totalElapsed := time.Since(probe.StartedAt)
	a.recorder.RecordRun(ctx, input, probe, transcode, mediaInfo, transcode.HardwareAccelerated, totalElapsed)

	return nil
}

// finalizeInvalid runs the cleanup-and-metrics path for a file that the probe
// activity determined was not valid media. The probe activity has already
// removed the source file in this case; cleanup here is a no-op for the file
// itself (RunCleanup tolerates ErrNotExist) but preserves the existing
// PreserveSource sentinel semantics.
func (a *Activities) finalizeInvalid(ctx context.Context, fin FinalizeInput) error {
	input := fin.Input

	if input.PreserveSource {
		if err := steps.WriteSentinel(input.FilePath); err != nil {
			return err
		}
	} else if err := steps.RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories); err != nil {
		return err
	}

	a.recorder.RecordInvalidFile(ctx, input.MediaType, input.MappingName)

	return nil
}

// finalizeFailure sends the configured failure webhook for a workflow that
// returned an error. The single failed step + error message are wrapped in the
// existing map[string]string shape so the wire payload matches today's.
func (a *Activities) finalizeFailure(ctx context.Context, fin FinalizeInput) error {
	stepErrors := map[string]string{}
	if fin.FailureStep != "" {
		stepErrors[fin.FailureStep] = fin.FailureErr
	}

	return steps.NotifyWorkflowFailure(ctx, stepErrors, MediaWorkflowName, fin.Input.FilePath, a.webhookClient)
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
