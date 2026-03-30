// Package media provides the Hatchet workflow definition for processing media files
// (movies and TV episodes) using a single parameterised workflow.
package media

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/shared"
)

const (
	MediaWorkflowName = "Media"
	// defaultTaskRetries is the number of retry attempts for retriable workflow steps.
	defaultTaskRetries = 3
)

// MediaWorkflowConfig holds the configuration for the media processing workflow.
type MediaWorkflowConfig struct {
	// OutputDir is the local directory where transcoded files are written.
	OutputDir string
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
}

// MediaInput is the workflow's trigger payload.
type MediaInput struct {
	FilePath    string             `json:"file_path"`
	MediaType   medialib.MediaType `json:"media_type"`
	MappingName string             `json:"mapping_name"`
}

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
	probeTask := wf.NewTask("probe", func(ctx hatchet.Context, input MediaInput) (shared.ProbeOutput, error) {
		start := time.Now()

		out, err := shared.RunProbe(ctx, input.FilePath)
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

	// transcode: re-encode or copy the video stream directly into cfg.OutputDir under a
	// temp name, then atomically rename it to the final path. Writing to the output
	// directory (rather than the system temp dir) means the rename is always within the
	// same filesystem, so it is guaranteed to be atomic on Linux.
	transcodeTask := wf.NewTask("transcode", func(ctx hatchet.Context, input MediaInput) (shared.TranscodeOutput, error) {
		var probe shared.ProbeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			return shared.TranscodeOutput{}, fmt.Errorf("get probe output: %w", err)
		}

		library, err := getArrLibrary(input.MediaType, radarrClient, sonarrClient)
		if err != nil {
			return shared.TranscodeOutput{}, fmt.Errorf("get arr library for artwork: %w", err)
		}

		out, err := shared.RunTranscode(ctx, input.FilePath, probe, cfg.OutputDir, cfg.HardwareDevicePath, library)
		if err == nil && out.ArtworkFetchSkipped {
			recorder.RecordArtworkFetchSkipped(ctx)
		}

		return out, err
	}, hatchet.WithParents(probeTask), skipIfInvalid)

	// notify: look up the media in Radarr (movie) or Sonarr (show) and trigger a library
	// rescan. Fails with ErrNotFound if the file is not recognised by the library service.
	notifyTask := wf.NewTask("notify", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		library, err := getArrLibrary(input.MediaType, radarrClient, sonarrClient)
		if err != nil {
			return struct{}{}, err
		}

		if err := library.RefreshByFilePath(ctx, input.FilePath); err != nil {
			return struct{}{}, fmt.Errorf("notify library: %w", err)
		}

		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, transcodeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// cleanup: delete the original source file after successful processing.
	cleanupTask := wf.NewTask("cleanup", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		return struct{}{}, shared.RunCleanup(input.FilePath)
	}, hatchet.WithParents(probeTask, notifyTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// record_metrics: record per-run OTel observations for valid-media completions.
	// Runs after cleanup so total_duration_seconds covers the full probe→cleanup span.
	_ = wf.NewTask("record_metrics", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		var probe shared.ProbeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			return struct{}{}, fmt.Errorf("get probe output for metrics: %w", err)
		}

		var transcode shared.TranscodeOutput
		if err := ctx.ParentOutput(transcodeTask, &transcode); err != nil {
			return struct{}{}, fmt.Errorf("get transcode output for metrics: %w", err)
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

		hardwareAccelerated := cfg.HardwareDevicePath != ""
		totalElapsed := time.Since(probe.StartedAt)
		recorder.RecordRun(ctx, input, probe, transcode, mediaInfo, hardwareAccelerated, totalElapsed)

		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, transcodeTask, notifyTask, cleanupTask), skipIfInvalid)

	// record_invalid: increment the invalid-files counter when probe determines the file
	// is not valid media. Skipped when the file is valid (i.e. the inverse of skipIfInvalid).
	_ = wf.NewTask("record_invalid", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		recorder.RecordInvalidFile(ctx, input.MediaType, input.MappingName)
		return struct{}{}, nil
	}, hatchet.WithParents(probeTask),
		hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == true")))

	// OnFailure: send a single aggregated failure notification to the configured webhook.
	wf.OnFailure(func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		return struct{}{}, shared.NotifyWorkflowFailure(ctx, ctx.StepRunErrors(), MediaWorkflowName, input.FilePath, webhookClient)
	})

	return wf
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
