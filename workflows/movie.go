// Package workflows provides Hatchet workflow definitions.
package workflows

import (
	"errors"
	"fmt"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

const movieWorkflowName = "MovieWorkflow"

// MovieWorkflowConfig holds the configuration for the movie processing workflow.
type MovieWorkflowConfig struct {
	// OutputDir is the local directory where transcoded files are written.
	OutputDir string
	// WebhookURL is the endpoint to notify on workflow failure.
	WebhookURL string
}

// MovieInput is the workflow's trigger payload.
type MovieInput struct {
	FilePath string `json:"file_path"`
}

// NewMovieWorkflow returns a Hatchet workflow that transcodes a movie file to the
// standard format, moves it to the output directory, and notifies Radarr.
//
// Steps (in order): probe → lookup → transcode → move → notify-radarr → cleanup.
// If the source file is not a valid media file with a video stream the file is
// deleted and all other steps are skipped without triggering the failure webhook.
func NewMovieWorkflow(
	client *hatchet.Client,
	cfg MovieWorkflowConfig,
	radarrClient medialib.MovieLibrary,
	webhookClient *webhook.Client,
) *hatchet.Workflow {
	maxRuns := int32(1)
	dropNewest := types.DropNewest

	wf := client.NewWorkflow(movieWorkflowName,
		hatchet.WithWorkflowConcurrency(types.Concurrency{
			Expression:    "input.file_path",
			MaxRuns:       &maxRuns,
			LimitStrategy: &dropNewest,
		}),
	)

	// probe: read codec/container info. Deletes the file and returns IsValidMedia=false
	// (without error) when the file is not a recognisable media file or has no video stream,
	// which causes all downstream steps to be skipped via WithSkipIf.
	probeTask := wf.NewTask("probe", runProbe)

	skipIfInvalid := hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == false"))

	// lookup: identify the movie in Radarr; fails with ErrNotFound if unrecognised.
	lookupTask := wf.NewTask("lookup", runLookup, hatchet.WithParents(probeTask), skipIfInvalid)

	// transcode: copy or re-encode the video stream, writing output to a temp path
	// in a workflow-run-specific subdirectory of the system temp directory.
	transcodeTask := wf.NewTask("transcode", func(ctx hatchet.Context, input MovieInput) (transcodeOutput, error) {
		var probe probeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			return transcodeOutput{}, fmt.Errorf("get probe output: %w", err)
		}

		return runTranscode(ctx, input, probe, ctx.WorkflowRunId())
	}, hatchet.WithParents(probeTask, lookupTask), skipIfInvalid)

	// move: copy the transcoded file to cfg.OutputDir and atomically rename it.
	// Copying first ensures cross-filesystem compatibility; renaming within the same
	// directory is atomic on Linux.
	moveTask := wf.NewTask("move", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		var tc transcodeOutput
		if err := ctx.ParentOutput(transcodeTask, &tc); err != nil {
			return struct{}{}, fmt.Errorf("get transcode output: %w", err)
		}

		return struct{}{}, runMove(input, tc, cfg.OutputDir)
	}, hatchet.WithParents(probeTask, transcodeTask), skipIfInvalid)

	// notify-radarr: trigger a Radarr library rescan for the movie.
	notifyTask := wf.NewTask("notify-radarr", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		var lu lookupOutput
		if err := ctx.ParentOutput(lookupTask, &lu); err != nil {
			return struct{}{}, fmt.Errorf("get lookup output: %w", err)
		}

		return struct{}{}, runNotify(ctx, lu, radarrClient)
	}, hatchet.WithParents(probeTask, lookupTask, moveTask), skipIfInvalid)

	// cleanup: delete the original source file after successful processing.
	_ = wf.NewTask("cleanup", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		return struct{}{}, runCleanup(input.FilePath)
	}, hatchet.WithParents(probeTask, notifyTask), skipIfInvalid)

	// OnFailure: send a failure notification to the configured webhook.
	wf.OnFailure(func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		for stepName, errMsg := range ctx.StepRunErrors() {
			_ = webhookClient.NotifyFailure(ctx, webhook.FailureEvent{
				Workflow: movieWorkflowName,
				FilePath: input.FilePath,
				Step:     stepName,
				Err:      errors.New(errMsg),
			})
		}

		return struct{}{}, nil
	})

	return wf
}
