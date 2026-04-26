// Package media provides the Hatchet workflow definition for processing media files
// (movies and TV episodes) using a single parameterised workflow.
package media

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// MediaWorkflowName re-exports the constant from the types package for callers that
// import workflows/media directly.
const MediaWorkflowName = mediatypes.MediaWorkflowName

const (
	// defaultTaskRetries is the number of retry attempts for retriable workflow steps.
	defaultTaskRetries = 3

	// DefaultDetectCropTimeout is the default Hatchet execution timeout for the
	// detectcrop step, used when MediaWorkflowConfig.DetectCropTimeout is zero.
	DefaultDetectCropTimeout = 30 * time.Minute
	// DefaultTranscodeTimeout is the default Hatchet execution timeout for the
	// transcode step, used when MediaWorkflowConfig.TranscodeTimeout is zero.
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
	// 0 means no minimum (any detected crop is applied). Defaults are applied by
	// the caller (e.g. cmd/worker via parseCropThreshold) before constructing this config.
	MinCropX int
	// MinCropY is the minimum number of pixels that must be trimmed vertically
	// for a crop to be applied. -1 disables the threshold (any crop is accepted).
	// 0 means no minimum (any detected crop is applied). Defaults are applied by
	// the caller (e.g. cmd/worker via parseCropThreshold) before constructing this config.
	MinCropY int
	// DetectCropTimeout is the Hatchet execution timeout for the detectcrop step.
	// When zero, NewMediaWorkflow applies a default of 30 minutes. Set by the caller
	// (e.g. cmd/worker via parseTimeout from MEDIA_DETECT_CROP_TIMEOUT).
	DetectCropTimeout time.Duration
	// TranscodeTimeout is the Hatchet execution timeout for the transcode step.
	// When zero, NewMediaWorkflow applies a default of 4 hours. Set by the caller
	// (e.g. cmd/worker via parseTimeout from MEDIA_TRANSCODE_TIMEOUT).
	TranscodeTimeout time.Duration
	// H265CRF is the constant-quality value passed to H.265 encoders. 0 means
	// use the encoder's built-in default. For libx265 this is the CRF; for
	// hevc_nvenc it is the CQ value; for hevc_qsv and hevc_vaapi it is the
	// global_quality (ICQ) value. Set by the caller (e.g. cmd/worker via
	// MEDIA_H265_CRF).
	H265CRF int
	// ProgressLogInterval controls how often a progress log line is emitted
	// during transcoding. Zero disables progress logging. Set by the caller
	// (e.g. cmd/worker via MEDIA_PROGRESS_LOG_INTERVAL).
	ProgressLogInterval time.Duration
}

// MediaInput is an alias for the shared input type so existing callers within this
// package do not need to be updated.
type MediaInput = mediatypes.MediaInput

// NewMediaWorkflow returns a Hatchet workflow that transcodes a media file (movie or TV
// episode) to the standard format, moves it to the output directory, and notifies the
// appropriate library service (Radarr for movies, Sonarr for TV episodes).
//
// Steps (in order): probe → transcode → notify → cleanup → record_metrics.
// A parallel record_invalid step fires only when the file is not valid media.
// If the source file is not a valid media file with a video stream the file is
// deleted and all other steps are skipped without triggering the failure webhook.
func NewMediaWorkflow(
	client *hatchet.Client,
	cfg MediaWorkflowConfig,
	radarrClient medialib.ArrLibrary,
	sonarrClient medialib.ArrLibrary,
	webhookClient *webhook.Client,
) *hatchet.Workflow {
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
		// Instrument registration errors are non-fatal: log for observability and fall
		// back to a noop recorder so the workflow can still run without metrics.
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

	maxRuns := int32(1)
	cancelNewest := types.CancelNewest

	wf := client.NewWorkflow(MediaWorkflowName,
		hatchet.WithWorkflowConcurrency(types.Concurrency{
			Expression:    "input.file_path",
			MaxRuns:       &maxRuns,
			LimitStrategy: &cancelNewest,
		}),
	)

	// probe: read codec/container info. Deletes the file and returns IsValidMedia=false
	// (without error) when the file is not a recognisable media file or has no video stream,
	// which causes all downstream steps to be skipped via WithSkipIf.
	// StartedAt is set here (not inside RunProbe) so existing RunProbe tests are unaffected.
	probeTask := wf.NewTask("probe", func(ctx hatchet.Context, input MediaInput) (steps.ProbeOutput, error) {
		start := time.Now()

		slog.InfoContext(ctx, "processing file", slog.String("file", input.FilePath))

		out, err := steps.RunProbe(ctx, input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
		logStepResult(ctx, "probe", input.FilePath, start, err)

		if err != nil {
			return out, err
		}

		out.StartedAt = start

		return out, nil
	})

	// skipIfInvalid must list probeTask as a direct WithParents entry on every step that
	// uses it. Hatchet only evaluates a PARENT_OVERRIDE (skip/wait) condition when the
	// referenced task appears in the step's direct-parent list — indirect ancestors are
	// not checked. Verified in hatchet/pkg/repository/trigger.go.
	skipIfInvalid := hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == false"))

	// detectcrop: run the ffmpeg cropdetect filter to find black bars. Returns a nil
	// Crop pointer when no crop is warranted (both axes disabled or trim below threshold).
	// cfg.MinCropX and cfg.MinCropY are passed directly; defaults are applied by the
	// caller (cmd/worker) via parseCropThreshold before constructing MediaWorkflowConfig.
	detectcropTask := wf.NewTask("detectcrop", func(ctx hatchet.Context, input MediaInput) (steps.DetectCropOutput, error) {
		start := time.Now()

		var probe steps.ProbeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			wrappedErr := fmt.Errorf("get probe output: %w", err)
			logStepResult(ctx, "detectcrop", input.FilePath, start, wrappedErr)

			return steps.DetectCropOutput{}, wrappedErr
		}

		crop, err := steps.RunDetectCrop(ctx, input.FilePath, probe.VideoWidth, probe.VideoHeight, cfg.MinCropX, cfg.MinCropY)
		logStepResult(ctx, "detectcrop", input.FilePath, start, err)

		if err != nil {
			return steps.DetectCropOutput{}, err
		}

		return steps.DetectCropOutput{Crop: crop}, nil
	}, hatchet.WithParents(probeTask), skipIfInvalid, hatchet.WithExecutionTimeout(cfg.DetectCropTimeout))

	// transcode: re-encode or copy the video stream directly into cfg.OutputDir under a
	// temp name, then atomically rename it to the final path. Writing to the output
	// directory (rather than the system temp dir) means the rename is always within the
	// same filesystem, so it is guaranteed to be atomic on Linux.
	transcodeTask := wf.NewTask("transcode", func(ctx hatchet.Context, input MediaInput) (steps.TranscodeOutput, error) {
		start := time.Now()

		var probe steps.ProbeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			wrappedErr := fmt.Errorf("get probe output: %w", err)
			logStepResult(ctx, "transcode", input.FilePath, start, wrappedErr)

			return steps.TranscodeOutput{}, wrappedErr
		}

		var detectcrop steps.DetectCropOutput
		if err := ctx.ParentOutput(detectcropTask, &detectcrop); err != nil {
			wrappedErr := fmt.Errorf("get detectcrop output: %w", err)
			logStepResult(ctx, "transcode", input.FilePath, start, wrappedErr)

			return steps.TranscodeOutput{}, wrappedErr
		}

		library, err := getArrLibrary(input.MediaType, radarrClient, sonarrClient)
		if err != nil {
			wrappedErr := fmt.Errorf("get arr library for artwork: %w", err)
			logStepResult(ctx, "transcode", input.FilePath, start, wrappedErr)

			return steps.TranscodeOutput{}, wrappedErr
		}

		if strings.TrimSpace(input.OutputPath) == "" {
			err := fmt.Errorf("output_path is required")
			logStepResult(ctx, "transcode", input.FilePath, start, err)

			return steps.TranscodeOutput{}, err
		}

		out, err := steps.RunTranscode(ctx, input.FilePath, probe, detectcrop.Crop, input.OutputPath, input.WatchRoot, cfg.HardwareDevicePath, cfg.H265CRF, cfg.ProgressLogInterval, library)
		if err == nil && out.ArtworkFetchSkipped {
			recorder.RecordArtworkFetchSkipped(ctx)
		}

		logStepResult(ctx, "transcode", input.FilePath, start, err)

		return out, err
	}, hatchet.WithParents(probeTask, detectcropTask), skipIfInvalid, hatchet.WithExecutionTimeout(cfg.TranscodeTimeout))

	// notify: send a DownloadedMoviesScan/DownloadedEpisodesScan command to Radarr/Sonarr
	// for the processed output file, triggering import into the library. The import path is
	// the transcoded output file path. When output.remotePath is set, the output.path prefix
	// is replaced by output.remotePath so the arr service sees its own mount point.
	notifyTask := wf.NewTask("notify", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		start := time.Now()

		var transcode steps.TranscodeOutput
		if err := ctx.ParentOutput(transcodeTask, &transcode); err != nil {
			wrappedErr := fmt.Errorf("get transcode output: %w", err)
			logStepResult(ctx, "notify", input.FilePath, start, wrappedErr)

			return struct{}{}, wrappedErr
		}

		library, err := getArrLibrary(input.MediaType, radarrClient, sonarrClient)
		if err != nil {
			logStepResult(ctx, "notify", input.FilePath, start, err)
			return struct{}{}, err
		}

		importPath := transcode.DestFilePath
		if input.OutputRemotePath != "" {
			after, ok := strings.CutPrefix(importPath, input.OutputPath)
			if !ok {
				wrappedErr := fmt.Errorf("output file %q does not start with output_path %q; cannot apply output_remote_path substitution", importPath, input.OutputPath)
				logStepResult(ctx, "notify", input.FilePath, start, wrappedErr)

				return struct{}{}, wrappedErr
			}

			importPath = input.OutputRemotePath + after
		}

		if err := library.ImportByFilePath(ctx, importPath); err != nil {
			wrappedErr := fmt.Errorf("notify library: %w", err)
			logStepResult(ctx, "notify", input.FilePath, start, wrappedErr)

			return struct{}{}, wrappedErr
		}

		logStepResult(ctx, "notify", input.FilePath, start, nil)

		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, transcodeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// finalize: write a sentinel or delete the source file after successful processing.
	// When PreserveSource is true, writes a .BASENAME.done sentinel so the watcher
	// skips the file on subsequent scans. When false, deletes the source file.
	finalizeTask := wf.NewTask("finalize", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		start := time.Now()

		var err error
		if input.PreserveSource {
			err = steps.WriteSentinel(input.FilePath)
		} else {
			err = steps.RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
		}

		logStepResult(ctx, "finalize", input.FilePath, start, err)

		return struct{}{}, err
	}, hatchet.WithParents(probeTask, notifyTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// record_metrics: record per-run OTel observations for valid-media completions.
	// Runs after finalize so total_duration_seconds covers the full probe→finalize span.
	_ = wf.NewTask("record_metrics", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		start := time.Now()

		var probe steps.ProbeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			wrappedErr := fmt.Errorf("get probe output for metrics: %w", err)
			logStepResult(ctx, "record_metrics", input.FilePath, start, wrappedErr)

			return struct{}{}, wrappedErr
		}

		var transcode steps.TranscodeOutput
		if err := ctx.ParentOutput(transcodeTask, &transcode); err != nil {
			wrappedErr := fmt.Errorf("get transcode output for metrics: %w", err)
			logStepResult(ctx, "record_metrics", input.FilePath, start, wrappedErr)

			return struct{}{}, wrappedErr
		}

		var mediaInfo medialib.MediaInfo

		if cfg.HighCardinalityLabels {
			library, libErr := getArrLibrary(input.MediaType, radarrClient, sonarrClient)
			if libErr == nil {
				info, infoErr := library.GetInfo(ctx, input.FilePath)
				if infoErr != nil {
					// GetInfo failure is best-effort: log it and count it, but do not
					// return an error. The step continues and records metrics without
					// high-cardinality labels rather than failing the workflow run.
					recorder.RecordMetricsError(ctx, fmt.Errorf("GetInfo: %w", infoErr))
				} else {
					mediaInfo = info
				}
			}
		}

		hardwareAccelerated := transcode.HardwareAccelerated
		totalElapsed := time.Since(probe.StartedAt)
		recorder.RecordRun(ctx, input, probe, transcode, mediaInfo, hardwareAccelerated, totalElapsed)

		logStepResult(ctx, "record_metrics", input.FilePath, start, nil)

		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, transcodeTask, notifyTask, finalizeTask), skipIfInvalid)

	// record_invalid: increment the invalid-files counter and then write a sentinel or
	// delete the source file when probe determines the file is not valid media.
	// Skipped when the file is valid (i.e. the inverse of skipIfInvalid).
	_ = wf.NewTask("record_invalid", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		start := time.Now()

		recorder.RecordInvalidFile(ctx, input.MediaType, input.MappingName)

		var err error
		if input.PreserveSource {
			err = steps.WriteSentinel(input.FilePath)
		} else {
			err = steps.RunCleanup(input.FilePath, input.WatchRoot, input.RetainEmptyDirectories)
		}

		logStepResult(ctx, "record_invalid", input.FilePath, start, err)

		return struct{}{}, err
	}, hatchet.WithParents(probeTask),
		hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == true")))

	// OnFailure: send a single aggregated failure notification to the configured webhook.
	wf.OnFailure(func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		start := time.Now()
		err := steps.NotifyWorkflowFailure(ctx, ctx.StepRunErrors(), MediaWorkflowName, input.FilePath, webhookClient)
		logStepResult(ctx, "onfailure", input.FilePath, start, err)

		return struct{}{}, err
	})

	return wf
}

func logStepResult(ctx context.Context, stepName, filePath string, start time.Time, err error) {
	if err != nil {
		slog.ErrorContext(ctx, "step failed", slog.String("step", stepName), slog.String("file", filePath), slog.Any("error", err))
		return
	}

	slog.InfoContext(ctx, "step complete", slog.String("step", stepName), slog.String("file", filePath), slog.Duration("elapsed", time.Since(start)))
}

// getArrLibrary returns the LibraryClient corresponding to mediaType, using
// radarrClient for movies and sonarrClient for TV episodes. It is the single
// dispatch point for media-type selection in the workflow.
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
