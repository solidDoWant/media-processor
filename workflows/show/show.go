// Package show provides the Hatchet workflow definition for processing TV episode files.
package show

import (
	"fmt"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/shared"
)

const (
	showWorkflowName = "ShowWorkflow"
	// defaultTaskRetries is the number of retry attempts for retriable workflow steps.
	defaultTaskRetries = 3
)

// ShowWorkflowConfig holds the configuration for the show processing workflow.
type ShowWorkflowConfig struct {
	// OutputDir is the local directory where transcoded files are written.
	OutputDir string
	// WebhookURL is the endpoint to notify on workflow failure.
	WebhookURL string
}

// ShowInput is the workflow's trigger payload.
type ShowInput struct {
	FilePath string `json:"file_path"`
}

// NewShowWorkflow returns a Hatchet workflow that transcodes a TV episode file to the
// standard format, moves it to the output directory, and notifies Sonarr.
//
// Steps (in order): probe → lookup → transcode → notify-sonarr → cleanup.
// If the source file is not a valid media file with a video stream the file is
// deleted and all other steps are skipped without triggering the failure webhook.
func NewShowWorkflow(
	client *hatchet.Client,
	cfg ShowWorkflowConfig,
	sonarrClient medialib.EpisodeLibrary,
	webhookClient *webhook.Client,
) *hatchet.Workflow {
	maxRuns := int32(1)
	cancelNewest := types.CancelNewest

	wf := client.NewWorkflow(showWorkflowName,
		hatchet.WithWorkflowConcurrency(types.Concurrency{
			Expression:    "input.file_path",
			MaxRuns:       &maxRuns,
			LimitStrategy: &cancelNewest,
		}),
	)

	// probe: read codec/container info. Deletes the file and returns IsValidMedia=false
	// (without error) when the file is not a recognisable media file or has no video stream,
	// which causes all downstream steps to be skipped via WithSkipIf.
	probeTask := wf.NewTask("probe", func(ctx hatchet.Context, input ShowInput) (shared.ProbeOutput, error) {
		return shared.RunProbe(ctx, input.FilePath)
	})

	// skipIfInvalid must list probeTask as a direct WithParents entry on every step that
	// uses it. Hatchet only evaluates a PARENT_OVERRIDE (skip/wait) condition when the
	// referenced task appears in the step's direct-parent list — indirect ancestors are
	// not checked. Verified in hatchet/pkg/repository/trigger.go.
	skipIfInvalid := hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == false"))

	// lookup: identify the episode in Sonarr; fails with ErrNotFound if unrecognised.
	lookupTask := wf.NewTask("lookup", func(ctx hatchet.Context, input ShowInput) (lookupOutput, error) {
		return runLookup(ctx, input.FilePath, sonarrClient)
	}, hatchet.WithParents(probeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// transcode: re-encode or copy the video stream directly into cfg.OutputDir under a
	// temp name, then atomically rename it to the final path. Writing to the output
	// directory (rather than the system temp dir) means the rename is always within the
	// same filesystem, so it is guaranteed to be atomic on Linux.
	transcodeTask := wf.NewTask("transcode", func(ctx hatchet.Context, input ShowInput) (struct{}, error) {
		var probe shared.ProbeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			return struct{}{}, fmt.Errorf("get probe output: %w", err)
		}

		return struct{}{}, shared.RunTranscode(ctx, input.FilePath, probe.VideoCodec, probe.Format, cfg.OutputDir)
	}, hatchet.WithParents(probeTask, lookupTask), skipIfInvalid)

	// notify-sonarr: trigger a Sonarr library rescan for the series.
	notifyTask := wf.NewTask("notify-sonarr", func(ctx hatchet.Context, input ShowInput) (struct{}, error) {
		var lu lookupOutput
		if err := ctx.ParentOutput(lookupTask, &lu); err != nil {
			return struct{}{}, fmt.Errorf("get lookup output: %w", err)
		}

		if err := sonarrClient.RefreshSeries(ctx, lu.SeriesID); err != nil {
			return struct{}{}, fmt.Errorf("notify sonarr: %w", err)
		}
		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, lookupTask, transcodeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// cleanup: delete the original source file after successful processing.
	_ = wf.NewTask("cleanup", func(ctx hatchet.Context, input ShowInput) (struct{}, error) {
		return struct{}{}, shared.RunCleanup(input.FilePath)
	}, hatchet.WithParents(probeTask, notifyTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// OnFailure: send a single aggregated failure notification to the configured webhook.
	wf.OnFailure(func(ctx hatchet.Context, input ShowInput) (struct{}, error) {
		return struct{}{}, shared.NotifyWorkflowFailure(ctx, ctx.StepRunErrors(), showWorkflowName, input.FilePath, webhookClient)
	})

	return wf
}
