// Package movie provides the Hatchet workflow definition for processing movie files.
package movie

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/webhook"
)

const (
	movieWorkflowName = "MovieWorkflow"
	// defaultTaskRetries is the number of retry attempts for retriable workflow steps.
	defaultTaskRetries = 3
)

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
// Steps (in order): probe → lookup → transcode → notify-radarr → cleanup.
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

	// skipIfInvalid must list probeTask as a direct WithParents entry on every step that
	// uses it. Hatchet only evaluates a PARENT_OVERRIDE (skip/wait) condition when the
	// referenced task appears in the step's direct-parent list — indirect ancestors are
	// not checked. Verified in hatchet/pkg/repository/trigger.go.
	skipIfInvalid := hatchet.WithSkipIf(hatchet.ParentCondition(probeTask, "output.is_valid_media == false"))

	// lookup: identify the movie in Radarr; fails with ErrNotFound if unrecognised.
	lookupTask := wf.NewTask("lookup", runLookup, hatchet.WithParents(probeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// transcode: re-encode or copy the video stream directly into cfg.OutputDir under a
	// temp name, then atomically rename it to the final path. Writing to the output
	// directory (rather than the system temp dir) means the rename is always within the
	// same filesystem, so it is guaranteed to be atomic on Linux.
	transcodeTask := wf.NewTask("transcode", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		var probe probeOutput
		if err := ctx.ParentOutput(probeTask, &probe); err != nil {
			return struct{}{}, fmt.Errorf("get probe output: %w", err)
		}

		return struct{}{}, runTranscode(ctx, input, probe, cfg.OutputDir)
	}, hatchet.WithParents(probeTask, lookupTask), skipIfInvalid)

	// notify-radarr: trigger a Radarr library rescan for the movie.
	notifyTask := wf.NewTask("notify-radarr", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		var lu lookupOutput
		if err := ctx.ParentOutput(lookupTask, &lu); err != nil {
			return struct{}{}, fmt.Errorf("get lookup output: %w", err)
		}

		if err := radarrClient.RefreshMovie(ctx, lu.MovieID); err != nil {
			return struct{}{}, fmt.Errorf("notify radarr: %w", err)
		}
		return struct{}{}, nil
	}, hatchet.WithParents(probeTask, lookupTask, transcodeTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// cleanup: delete the original source file after successful processing.
	_ = wf.NewTask("cleanup", func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		return struct{}{}, runCleanup(input.FilePath)
	}, hatchet.WithParents(probeTask, notifyTask), skipIfInvalid, hatchet.WithRetries(defaultTaskRetries))

	// OnFailure: send a single aggregated failure notification to the configured webhook.
	wf.OnFailure(func(ctx hatchet.Context, input MovieInput) (struct{}, error) {
		stepErrors := ctx.StepRunErrors()
		if len(stepErrors) == 0 {
			return struct{}{}, nil
		}

		errs := make([]error, 0, len(stepErrors))
		steps := make([]string, 0, len(stepErrors))
		for stepName, errMsg := range stepErrors {
			steps = append(steps, stepName)
			errs = append(errs, fmt.Errorf("%s: %s", stepName, errMsg))
		}

		if err := webhookClient.NotifyFailure(ctx, webhook.FailureEvent{
			Workflow: movieWorkflowName,
			FilePath: input.FilePath,
			Step:     strings.Join(steps, ", "),
			Err:      errors.Join(errs...),
		}); err != nil {
			return struct{}{}, fmt.Errorf("notify failure: %w", err)
		}

		return struct{}{}, nil
	})

	return wf
}
