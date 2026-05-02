package media

import (
	"errors"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
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

// MediaWorkflow processes one media file end-to-end. The body is straight-line
// Go: probe, branch on validity, run the per-path activities. A defer block
// invokes the failure-webhook activity when the workflow returns an error.
//
// Each activity is invoked with the retry policy that fits its idempotency
// profile: idempotent operations (Notify, Cleanup) use retryableMaxAttempts
// so transient flakes do not fail the run; everything else (Probe,
// DetectCrop, Transcode, NotifyFailure) uses defaultMaxAttempts (single
// attempt) because retrying would either duplicate non-idempotent side
// effects or repeat expensive work that will not recover.
//
// Per-run application metrics are emitted by the activities themselves through
// the SDK MetricsHandler. End-to-end workflow latency comes for free from the
// SDK's temporal_workflow_endtoend_latency_seconds histogram (tagged with
// workflow_type), so the workflow body does not emit a duration metric.
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

		notifyErr := workflow.ExecuteActivity(failureCtx, NotifyFailureActivityName, input, step, message).Get(failureCtx, nil)
		if notifyErr != nil {
			log.Error("failure-webhook activity failed", "error", notifyErr.Error())
		}
	}()

	probeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultProbeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var probe ProbeOutput
	if err := workflow.ExecuteActivity(probeCtx, ProbeActivityName, input).Get(probeCtx, &probe); err != nil {
		return &stepError{step: "probe", err: err}
	}

	if !probe.IsValidMedia {
		// Invalid path. Probe has already removed the source file and emitted
		// the invalid-files counter; Cleanup here is a near-no-op for the file
		// (RunCleanup tolerates ErrNotExist) but still writes the .done
		// sentinel when PreserveSource is set.
		invalidCleanupCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: defaultFinalizeTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: retryableMaxAttempts},
		})

		if err := workflow.ExecuteActivity(invalidCleanupCtx, CleanupActivityName, input).Get(invalidCleanupCtx, nil); err != nil {
			return &stepError{step: "cleanup", err: err}
		}

		return nil
	}

	cropCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: a.cfg.DetectCropTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var crop DetectCropOutput
	if err := workflow.ExecuteActivity(cropCtx, DetectCropActivityName, input, probe).Get(cropCtx, &crop); err != nil {
		return &stepError{step: "detectcrop", err: err}
	}

	transcodeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: a.cfg.TranscodeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: defaultMaxAttempts},
	})

	var transcode TranscodeOutput
	if err := workflow.ExecuteActivity(transcodeCtx, TranscodeActivityName, input, probe, crop).Get(transcodeCtx, &transcode); err != nil {
		return &stepError{step: "transcode", err: err}
	}

	notifyCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultFinalizeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: retryableMaxAttempts},
	})

	if err := workflow.ExecuteActivity(notifyCtx, NotifyActivityName, input, transcode).Get(notifyCtx, nil); err != nil {
		return &stepError{step: "notify", err: err}
	}

	cleanupCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: defaultFinalizeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: retryableMaxAttempts},
	})

	if err := workflow.ExecuteActivity(cleanupCtx, CleanupActivityName, input).Get(cleanupCtx, nil); err != nil {
		return &stepError{step: "cleanup", err: err}
	}

	return nil
}
