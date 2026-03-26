// Package media provides the Hatchet workflow definition for processing media files
// (movies and TV episodes) using a single parameterised workflow.
package media

import (
	"fmt"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/shared"
)

const (
	MediaWorkflowName = "Media"
	// defaultTaskRetries is the number of retry attempts for retriable workflow steps.
	defaultTaskRetries = 3
)

// MediaType identifies whether a file is a movie or a TV show episode.
type MediaType string

const (
	Movie MediaType = "movie"
	Show  MediaType = "show"
)

// MediaWorkflowConfig holds the configuration for the media processing workflow.
type MediaWorkflowConfig struct {
	// OutputDir is the local directory where transcoded files are written.
	OutputDir string
	// WebhookURL is the endpoint to notify on workflow failure.
	WebhookURL string
}

// MediaInput is the workflow's trigger payload.
type MediaInput struct {
	FilePath  string    `json:"file_path"`
	MediaType MediaType `json:"media_type"`
}

// NewMediaWorkflow returns a Hatchet workflow that transcodes a media file (movie or TV
// episode) to the standard format, moves it to the output directory, and notifies the
// appropriate library service (Radarr for movies, Sonarr for TV episodes).
//
// Steps (in order): probe → lookup → transcode → notify → cleanup.
// If the source file is not a valid media file with a video stream the file is
// deleted and all other steps are skipped without triggering the failure webhook.
func NewMediaWorkflow(
	client *hatchet.Client,
	cfg MediaWorkflowConfig,
	radarrClient medialib.MovieLibrary,
	sonarrClient medialib.EpisodeLibrary,
	webhookClient *webhook.Client,
) *hatchet.Workflow {
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
	probeTask := wf.NewTask("probe", func(ctx hatchet.Context, input MediaInput) (shared.ProbeOutput, error) {
		return shared.RunProbe(ctx, input.FilePath)
	})

	// skipIfInvalid must list probeTask as a direct WithParents entry on every step that
	// uses it. Hatchet only evaluates a PARENT_OVERRIDE (skip/wait) condition when the
	// referenced task appears in the step's direct-parent list — indirect ancestors are
	// not checked. Verified in hatchet/pkg/repository/trigger.go.
	skipIfInvalid := hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == false"))

	// lookup: identify the media in Radarr (movie) or Sonarr (show); fails with ErrNotFound if unrecognised.
	lookupTask := wf.NewTask("lookup", func(ctx hatchet.Context, input MediaInput) (lookupOutput, error) {
		return runLookup(ctx, input.FilePath, input.MediaType, radarrClient, sonarrClient)
	}, hatchet.WithParents(probeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// transcode: re-encode or copy the video stream directly into cfg.OutputDir under a
	// temp name, then atomically rename it to the final path. Writing to the output
	// directory (rather than the system temp dir) means the rename is always within the
	// same filesystem, so it is guaranteed to be atomic on Linux.
	transcodeTask := wf.NewTask("transcode", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		var probe shared.ProbeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			return struct{}{}, fmt.Errorf("get probe output: %w", err)
		}

		return struct{}{}, shared.RunTranscode(ctx, input.FilePath, probe.VideoCodec, probe.Format, cfg.OutputDir)
	}, hatchet.WithParents(probeTask, lookupTask), skipIfInvalid)

	// notify: trigger a library rescan in Radarr (movie) or Sonarr (show).
	notifyTask := wf.NewTask("notify", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		var lu lookupOutput
		if err := ctx.ParentOutput(lookupTask, &lu); err != nil {
			return struct{}{}, fmt.Errorf("get lookup output: %w", err)
		}

		switch input.MediaType {
		case Movie:
			if err := radarrClient.RefreshMovie(ctx, lu.MediaID); err != nil {
				return struct{}{}, fmt.Errorf("notify radarr: %w", err)
			}
		case Show:
			if err := sonarrClient.RefreshSeries(ctx, lu.MediaID); err != nil {
				return struct{}{}, fmt.Errorf("notify sonarr: %w", err)
			}
		default:
			return struct{}{}, fmt.Errorf("unknown media type %q", input.MediaType)
		}
		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, lookupTask, transcodeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// cleanup: delete the original source file after successful processing.
	_ = wf.NewTask("cleanup", func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		return struct{}{}, shared.RunCleanup(input.FilePath)
	}, hatchet.WithParents(probeTask, notifyTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// OnFailure: send a single aggregated failure notification to the configured webhook.
	wf.OnFailure(func(ctx hatchet.Context, input MediaInput) (struct{}, error) {
		return struct{}{}, shared.NotifyWorkflowFailure(ctx, ctx.StepRunErrors(), MediaWorkflowName, input.FilePath, webhookClient)
	})

	return wf
}
