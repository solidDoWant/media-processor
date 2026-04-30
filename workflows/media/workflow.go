package media

import (
	"errors"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/media-processor/workflows/steps"
)

// stepError wraps an activity error with the step name where it originated.
// The workflow's failure-webhook defer extracts the step + underlying error
// via errors.As, removing the need for a mutable shared variable straddling
// the workflow body and the defer. The wrapped form ("step: message") also
// surfaces directly in the Temporal Web UI's workflow-failure pane.
type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.step + ": " + e.err.Error() }

func (e *stepError) Unwrap() error { return e.err }

// MediaWorkflow processes one media file: probe, optional detectcrop and
// transcode, finalize. A defer block sends a failure-webhook notification
// when the workflow exits with an error.
func (a *Activities) MediaWorkflow(ctx workflow.Context, input MediaInput) (err error) {
	log := workflow.GetLogger(ctx)
	log.Info("processing file", "file", input.FilePath)

	defer func() {
		if err == nil {
			return
		}

		step, message := "unknown", err.Error()

		var sErr *stepError
		if errors.As(err, &sErr) {
			step, message = sErr.step, sErr.err.Error()
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
			FailureStep: step,
			FailureErr:  message,
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
	if err := workflow.ExecuteActivity(probeCtx, ProbeActivityName, input).Get(probeCtx, &probe); err != nil {
		return &stepError{step: "probe", err: err}
	}

	if !probe.IsValidMedia {
		invalidCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: defaultFinalizeTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
		})

		if err := workflow.ExecuteActivity(invalidCtx, FinalizeActivityName, FinalizeInput{
			Mode:  FinalizeInvalid,
			Input: input,
			Probe: probe,
		}).Get(invalidCtx, nil); err != nil {
			return &stepError{step: "finalize", err: err}
		}

		return nil
	}

	cropCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: a.cfg.DetectCropTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var crop steps.DetectCropOutput
	if err := workflow.ExecuteActivity(cropCtx, DetectCropActivityName, input, probe).Get(cropCtx, &crop); err != nil {
		return &stepError{step: "detectcrop", err: err}
	}

	transcodeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: a.cfg.TranscodeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var transcode steps.TranscodeOutput
	if err := workflow.ExecuteActivity(transcodeCtx, TranscodeActivityName, input, probe, crop).Get(transcodeCtx, &transcode); err != nil {
		return &stepError{step: "transcode", err: err}
	}

	finalizeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultFinalizeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: finalizeValidMaxAttempts},
	})

	if err := workflow.ExecuteActivity(finalizeCtx, FinalizeActivityName, FinalizeInput{
		Mode:      FinalizeValid,
		Input:     input,
		Probe:     probe,
		Transcode: transcode,
	}).Get(finalizeCtx, nil); err != nil {
		return &stepError{step: "finalize", err: err}
	}

	return nil
}
