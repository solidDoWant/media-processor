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
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// MediaWorkflowName re-exports the constant from the types package for callers that
// import workflows/media directly.
const MediaWorkflowName = mediatypes.MediaWorkflowName

const (
	// defaultActivityRetries is the number of retry attempts for retriable activities.
	defaultActivityRetries int32 = 3

	// DefaultDetectCropTimeout is the default Temporal StartToCloseTimeout for the
	// detectcrop activity, used when MediaWorkflowConfig.DetectCropTimeout is zero.
	DefaultDetectCropTimeout = 30 * time.Minute
	// DefaultTranscodeTimeout is the default Temporal StartToCloseTimeout for the
	// transcode activity, used when MediaWorkflowConfig.TranscodeTimeout is zero.
	DefaultTranscodeTimeout = 4 * time.Hour
)

// MediaWorkflowConfig holds the configuration for the media processing workflow.
type MediaWorkflowConfig struct {
	// WebhookURL is the endpoint to notify on workflow failure.
	WebhookURL string
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
	MinCropX int
	// MinCropY is the minimum number of pixels that must be trimmed vertically
	// for a crop to be applied. -1 disables the threshold (any crop is accepted).
	MinCropY int
	// DetectCropTimeout is the Temporal StartToCloseTimeout for the detectcrop activity.
	// When zero, DefaultDetectCropTimeout is applied. Set by cmd/worker via parseTimeout.
	DetectCropTimeout time.Duration
	// TranscodeTimeout is the Temporal StartToCloseTimeout for the transcode activity.
	// When zero, DefaultTranscodeTimeout is applied. Set by cmd/worker via parseTimeout.
	TranscodeTimeout time.Duration
	// H265CRF is the constant-quality value passed to H.265 encoders. 0 means
	// use the encoder's built-in default.
	H265CRF int
	// ProgressLogInterval controls how often a progress log line is emitted
	// during transcoding. Zero disables progress logging.
	ProgressLogInterval time.Duration
}

// MediaInput is an alias for the shared input type so existing callers within this
// package do not need to be updated.
type MediaInput = mediatypes.MediaInput

// FinalizeInput is the input to the Finalize activity.
// When ProbeOut.IsValidMedia is false, TranscodeOut is the zero value and
// the activity records the invalid-file metric and cleans up. When true,
// it notifies the arr library, cleans up, and records run metrics.
type FinalizeInput struct {
	Input        MediaInput            `json:"input"`
	ProbeOut     steps.ProbeOutput     `json:"probe_out"`
	TranscodeOut steps.TranscodeOutput `json:"transcode_out,omitempty"`
}

// OnFailureInput is the input to the OnFailureWebhook activity.
type OnFailureInput struct {
	Input    MediaInput `json:"input"`
	ErrorMsg string     `json:"error_msg"`
}

// MediaWorkflows holds workflow config and provides the Temporal workflow function.
type MediaWorkflows struct {
	cfg MediaWorkflowConfig
}

// NewMediaWorkflows creates a MediaWorkflows with defaults applied to the config.
func NewMediaWorkflows(cfg MediaWorkflowConfig) *MediaWorkflows {
	if cfg.DetectCropTimeout == 0 {
		cfg.DetectCropTimeout = DefaultDetectCropTimeout
	}

	if cfg.TranscodeTimeout == 0 {
		cfg.TranscodeTimeout = DefaultTranscodeTimeout
	}

	return &MediaWorkflows{cfg: cfg}
}

// MediaWorkflow is the Temporal workflow function for media file processing.
//
// Execution path (valid media): Probe → DetectCrop → Transcode → Finalize
// Execution path (invalid media): Probe → Finalize (record_invalid + cleanup)
//
// When the workflow returns a non-nil error, a deferred OnFailureWebhook activity
// fires the failure webhook via MEDIA_WEBHOOK_URL.
func (mw *MediaWorkflows) MediaWorkflow(ctx workflow.Context, input MediaInput) (retErr error) {
	var ma *MediaActivities

	defer func() {
		if retErr == nil {
			return
		}

		disconnectedCtx, _ := workflow.NewDisconnectedContext(ctx)
		fCtx := workflow.WithActivityOptions(disconnectedCtx, workflow.ActivityOptions{
			StartToCloseTimeout: 2 * time.Minute,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		})
		_ = workflow.ExecuteActivity(fCtx, ma.OnFailureWebhook, OnFailureInput{
			Input:    input,
			ErrorMsg: retErr.Error(),
		}).Get(fCtx, nil)
	}()

	var probeOut steps.ProbeOutput

	pCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	if err := workflow.ExecuteActivity(pCtx, ma.Probe, input).Get(pCtx, &probeOut); err != nil {
		return fmt.Errorf("probe: %w", err)
	}

	finalizeOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultActivityRetries + 1},
	}

	if !probeOut.IsValidMedia {
		fCtx := workflow.WithActivityOptions(ctx, finalizeOpts)
		if err := workflow.ExecuteActivity(fCtx, ma.Finalize, FinalizeInput{
			Input:    input,
			ProbeOut: probeOut,
		}).Get(fCtx, nil); err != nil {
			return fmt.Errorf("finalize (invalid): %w", err)
		}

		return nil
	}

	var detectCropOut steps.DetectCropOutput

	dCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: mw.cfg.DetectCropTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	if err := workflow.ExecuteActivity(dCtx, ma.DetectCrop, input, probeOut).Get(dCtx, &detectCropOut); err != nil {
		return fmt.Errorf("detectcrop: %w", err)
	}

	var transcodeOut steps.TranscodeOutput

	tCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: mw.cfg.TranscodeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	if err := workflow.ExecuteActivity(tCtx, ma.Transcode, input, probeOut, detectCropOut).Get(tCtx, &transcodeOut); err != nil {
		return fmt.Errorf("transcode: %w", err)
	}

	fCtx := workflow.WithActivityOptions(ctx, finalizeOpts)
	if err := workflow.ExecuteActivity(fCtx, ma.Finalize, FinalizeInput{
		Input:        input,
		ProbeOut:     probeOut,
		TranscodeOut: transcodeOut,
	}).Get(fCtx, nil); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}

	return nil
}

// MediaActivities holds the dependencies for the media workflow activities.
type MediaActivities struct {
	cfg           MediaWorkflowConfig
	radarrClient  medialib.ArrLibrary
	sonarrClient  medialib.ArrLibrary
	webhookClient *webhook.Client
	recorder      *Recorder
	// transcodeSem limits concurrent transcode activities to 1 per worker process.
	transcodeSem chan struct{}
}

// NewMediaActivities creates a MediaActivities instance with defaults applied.
// Recorder creation errors are non-fatal and fall back to a noop recorder.
func NewMediaActivities(
	cfg MediaWorkflowConfig,
	radarrClient medialib.ArrLibrary,
	sonarrClient medialib.ArrLibrary,
	webhookClient *webhook.Client,
) *MediaActivities {
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
		slog.Warn("media: failed to create metrics recorder, falling back to noop", "error", err)

		var noopErr error

		recorder, noopErr = NewRecorder(noop.NewMeterProvider(), false)
		if noopErr != nil {
			// noop.NewMeterProvider() instrument registration never returns errors in
			// practice. If it somehow does, there is no safe fallback — panic to surface
			// the bug immediately rather than silently proceeding with a nil recorder.
			panic(fmt.Sprintf("media: failed to create noop metrics recorder: %v", noopErr))
		}
	}

	sem := make(chan struct{}, 1)
	sem <- struct{}{}

	return &MediaActivities{
		cfg:           cfg,
		radarrClient:  radarrClient,
		sonarrClient:  sonarrClient,
		webhookClient: webhookClient,
		recorder:      recorder,
		transcodeSem:  sem,
	}
}

// Probe reads codec and container info for the file in input. If the file is not valid
// media, RunProbe deletes it and returns IsValidMedia=false (without error); the workflow
// skips to Finalize(invalid). Sets ProbeOutput.StartedAt for downstream elapsed-time metrics.
func (ma *MediaActivities) Probe(ctx context.Context, input MediaInput) (steps.ProbeOutput, error) {
	start := time.Now()

	slog.InfoContext(ctx, "processing file", slog.String("file", input.FilePath))

	out, err := steps.RunProbe(ctx, input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
	logActivityResult(ctx, "probe", input.FilePath, start, err)

	if err != nil {
		return out, err
	}

	out.StartedAt = start

	return out, nil
}

// DetectCrop runs the ffmpeg cropdetect filter to find black bars. Returns a zero-value
// DetectCropOutput (nil Crop) when no crop is warranted.
func (ma *MediaActivities) DetectCrop(ctx context.Context, input MediaInput, probe steps.ProbeOutput) (steps.DetectCropOutput, error) {
	start := time.Now()

	crop, err := steps.RunDetectCrop(ctx, input.FilePath, probe.VideoWidth, probe.VideoHeight, ma.cfg.MinCropX, ma.cfg.MinCropY)
	logActivityResult(ctx, "detectcrop", input.FilePath, start, err)

	if err != nil {
		return steps.DetectCropOutput{}, err
	}

	return steps.DetectCropOutput{Crop: crop}, nil
}

// Transcode re-encodes the media file into the output directory, acquiring a per-process
// semaphore so that at most one transcode runs at a time on each worker.
func (ma *MediaActivities) Transcode(ctx context.Context, input MediaInput, probe steps.ProbeOutput, detectCrop steps.DetectCropOutput) (steps.TranscodeOutput, error) {
	select {
	case <-ma.transcodeSem:
	case <-ctx.Done():
		return steps.TranscodeOutput{}, ctx.Err()
	}

	defer func() { ma.transcodeSem <- struct{}{} }()

	start := time.Now()

	library, err := getArrLibrary(input.MediaType, ma.radarrClient, ma.sonarrClient)
	if err != nil {
		wrappedErr := fmt.Errorf("get arr library for artwork: %w", err)
		logActivityResult(ctx, "transcode", input.FilePath, start, wrappedErr)

		return steps.TranscodeOutput{}, wrappedErr
	}

	outputPath := filepath.Clean(strings.TrimSpace(input.OutputPath))
	if outputPath == "" || outputPath == "." {
		err := fmt.Errorf("output_path is required")
		logActivityResult(ctx, "transcode", input.FilePath, start, err)

		return steps.TranscodeOutput{}, err
	}

	out, err := steps.RunTranscode(ctx, input.FilePath, probe, detectCrop.Crop, outputPath, input.WatchRoot, ma.cfg.HardwareDevicePath, ma.cfg.H265CRF, ma.cfg.ProgressLogInterval, library)
	if err == nil && out.ArtworkFetchSkipped {
		ma.recorder.RecordArtworkFetchSkipped(ctx)
	}

	logActivityResult(ctx, "transcode", input.FilePath, start, err)

	return out, err
}

// Finalize is the final activity for both the valid and invalid media paths.
//
// Invalid media (fin.ProbeOut.IsValidMedia == false): records the invalid-file metric
// and writes a sentinel or deletes the source file.
//
// Valid media: notifies the arr library (Radarr/Sonarr), writes a sentinel or deletes
// the source file, and records per-run OTel metrics.
func (ma *MediaActivities) Finalize(ctx context.Context, fin FinalizeInput) error {
	input := fin.Input
	probe := fin.ProbeOut

	if !probe.IsValidMedia {
		start := time.Now()

		ma.recorder.RecordInvalidFile(ctx, input.MediaType, input.MappingName)

		var err error
		if input.PreserveSource {
			err = steps.WriteSentinel(input.FilePath)
		} else {
			err = steps.RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
		}

		logActivityResult(ctx, "finalize(invalid)", input.FilePath, start, err)

		return err
	}

	transcodeOut := fin.TranscodeOut

	// Notify the arr library to import the transcoded file.
	notifyStart := time.Now()

	library, err := getArrLibrary(input.MediaType, ma.radarrClient, ma.sonarrClient)
	if err != nil {
		logActivityResult(ctx, "finalize(notify)", input.FilePath, notifyStart, err)
		return err
	}

	importPath := transcodeOut.DestFilePath

	if remotePath := strings.TrimSpace(input.OutputRemotePath); remotePath != "" {
		outputPath := filepath.Clean(strings.TrimSpace(input.OutputPath))

		rel, relErr := filepath.Rel(outputPath, importPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			err := fmt.Errorf("output file %q is not under output_path %q; cannot apply output_remote_path substitution", importPath, input.OutputPath)
			logActivityResult(ctx, "finalize(notify)", input.FilePath, notifyStart, err)

			return err
		}

		importPath = filepath.Join(remotePath, rel)
	}

	if err := library.ImportByFilePath(ctx, importPath); err != nil {
		wrappedErr := fmt.Errorf("notify library: %w", err)
		logActivityResult(ctx, "finalize(notify)", input.FilePath, notifyStart, wrappedErr)

		return wrappedErr
	}

	logActivityResult(ctx, "finalize(notify)", input.FilePath, notifyStart, nil)

	// Write sentinel or delete source file.
	cleanupStart := time.Now()

	var cleanupErr error
	if input.PreserveSource {
		cleanupErr = steps.WriteSentinel(input.FilePath)
	} else {
		cleanupErr = steps.RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
	}

	logActivityResult(ctx, "finalize(cleanup)", input.FilePath, cleanupStart, cleanupErr)

	if cleanupErr != nil {
		return cleanupErr
	}

	// Record per-run OTel metrics. GetInfo failure is best-effort: log and count,
	// but do not fail the activity.
	var mediaInfo medialib.MediaInfo

	if ma.cfg.HighCardinalityLabels {
		if lib, libErr := getArrLibrary(input.MediaType, ma.radarrClient, ma.sonarrClient); libErr == nil {
			info, infoErr := lib.GetInfo(ctx, input.FilePath)
			if infoErr != nil {
				ma.recorder.RecordMetricsError(ctx, fmt.Errorf("GetInfo: %w", infoErr))
			} else {
				mediaInfo = info
			}
		}
	}

	totalElapsed := time.Since(probe.StartedAt)
	ma.recorder.RecordRun(ctx, input, probe, transcodeOut, mediaInfo, transcodeOut.HardwareAccelerated, totalElapsed)

	return nil
}

// OnFailureWebhook fires the failure webhook when a workflow execution returns an error.
// It is called via a deferred block in MediaWorkflow and runs in a disconnected context
// so it executes even when the workflow context is cancelled.
func (ma *MediaActivities) OnFailureWebhook(ctx context.Context, fin OnFailureInput) error {
	start := time.Now()
	stepErrors := map[string]string{"workflow": fin.ErrorMsg}
	err := steps.NotifyWorkflowFailure(ctx, stepErrors, MediaWorkflowName, fin.Input.FilePath, ma.webhookClient)
	logActivityResult(ctx, "onfailure", fin.Input.FilePath, start, err)

	return err
}

func logActivityResult(ctx context.Context, stepName, filePath string, start time.Time, err error) {
	if err != nil {
		slog.ErrorContext(ctx, "activity failed", slog.String("step", stepName), slog.String("file", filePath), slog.Any("error", err))
		return
	}

	slog.InfoContext(ctx, "activity complete", slog.String("step", stepName), slog.String("file", filePath), slog.Duration("elapsed", time.Since(start)))
}

// getArrLibrary returns the ArrLibrary corresponding to mediaType.
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
