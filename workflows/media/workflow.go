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

	// The valid path is split across three Finalize invocations so each
	// sub-step gets the retry policy that matches its idempotency profile:
	// notify and cleanup retry on transient failures; metrics never retries
	// because histogram emission is not idempotent.

	notifyCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultFinalizeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: finalizeRetryableMaxAttempts},
	})

	if err := workflow.ExecuteActivity(notifyCtx, FinalizeActivityName, FinalizeInput{
		Mode:      FinalizeNotify,
		Input:     input,
		Probe:     probe,
		Transcode: transcode,
	}).Get(notifyCtx, nil); err != nil {
		return &stepError{step: "notify", err: err}
	}

	cleanupCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultFinalizeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: finalizeRetryableMaxAttempts},
	})

	if err := workflow.ExecuteActivity(cleanupCtx, FinalizeActivityName, FinalizeInput{
		Mode:  FinalizeCleanup,
		Input: input,
	}).Get(cleanupCtx, nil); err != nil {
		return &stepError{step: "cleanup", err: err}
	}

	// Metrics: best-effort. A failure here means an OTel SDK panic or the
	// activity being killed mid-record — neither is worth failing the
	// workflow over (and the firehose of possibly-partial samples already
	// emitted on the failed attempt cannot be unsent). Log and proceed.
	metricsCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultFinalizeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	if metricsErr := workflow.ExecuteActivity(metricsCtx, FinalizeActivityName, FinalizeInput{
		Mode:      FinalizeMetrics,
		Input:     input,
		Probe:     probe,
		Transcode: transcode,
	}).Get(metricsCtx, nil); metricsErr != nil {
		log.Warn("metrics activity failed; workflow proceeds", "error", metricsErr.Error())
	}

	return nil
}
