package media

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

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
