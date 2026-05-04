package media

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// defaultActivityOptions builds the StartToCloseTimeout/RetryPolicy pair used
// by every activity in the workflow. The two parameters cover every variation
// in use today: only the timeout and MaximumAttempts differ across call sites.
// The transcode activity additionally sets HeartbeatTimeout on the returned
// struct.
func defaultActivityOptions(timeout time.Duration, attempts int32) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: attempts},
	}
}

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

		var actErr *temporal.ActivityError
		if errors.As(err, &actErr) {
			step = actErr.ActivityType().Name
		}

		// The workflow's context is cancelled when the workflow returns an
		// error, so further activities must be scheduled on a disconnected
		// context to actually run.
		disconnected, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()

		failureCtx := workflow.WithActivityOptions(disconnected, defaultActivityOptions(defaultFinalizeTimeout, defaultMaxAttempts))

		notifyErr := workflow.ExecuteActivity(failureCtx, NotifyFailureActivityName, input, step, message).Get(failureCtx, nil)
		if notifyErr != nil {
			log.Error("failure-webhook activity failed", "error", notifyErr.Error())
		}
	}()

	probeCtx := workflow.WithActivityOptions(ctx, defaultActivityOptions(defaultProbeTimeout, defaultMaxAttempts))

	var probe ProbeOutput
	if err := workflow.ExecuteActivity(probeCtx, ProbeActivityName, input).Get(probeCtx, &probe); err != nil {
		return err
	}

	if !probe.IsValidMedia {
		// Invalid path. Probe has already removed the source file and emitted
		// the invalid-files counter; Cleanup here is a near-no-op for the file
		// (RunCleanup tolerates ErrNotExist) but still writes the .done
		// sentinel when PreserveSource is set. No transcode happened, so an
		// empty TranscodeOutput suppresses output-side pruning.
		invalidCleanupCtx := workflow.WithActivityOptions(ctx, defaultActivityOptions(defaultFinalizeTimeout, retryableMaxAttempts))

		if err := workflow.ExecuteActivity(invalidCleanupCtx, CleanupActivityName, input, TranscodeOutput{}).Get(invalidCleanupCtx, nil); err != nil {
			return err
		}

		return nil
	}

	cropCtx := workflow.WithActivityOptions(ctx, defaultActivityOptions(a.cfg.DetectCropTimeout, defaultMaxAttempts))

	var crop DetectCropOutput
	if err := workflow.ExecuteActivity(cropCtx, DetectCropActivityName, input, probe).Get(cropCtx, &crop); err != nil {
		return err
	}

	transcodeOpts := defaultActivityOptions(a.cfg.TranscodeTimeout, defaultMaxAttempts)
	transcodeOpts.HeartbeatTimeout = transcodeHeartbeatTimeout(a.cfg.ProgressLogInterval)
	transcodeCtx := workflow.WithActivityOptions(ctx, transcodeOpts)

	var transcode TranscodeOutput
	if err := workflow.ExecuteActivity(transcodeCtx, TranscodeActivityName, input, probe, crop).Get(transcodeCtx, &transcode); err != nil {
		return err
	}

	notifyCtx := workflow.WithActivityOptions(ctx, defaultActivityOptions(defaultNotifyTimeout, retryableMaxAttempts))

	if err := workflow.ExecuteActivity(notifyCtx, NotifyActivityName, input, transcode).Get(notifyCtx, nil); err != nil {
		return err
	}

	cleanupCtx := workflow.WithActivityOptions(ctx, defaultActivityOptions(defaultFinalizeTimeout, retryableMaxAttempts))

	if err := workflow.ExecuteActivity(cleanupCtx, CleanupActivityName, input, transcode).Get(cleanupCtx, nil); err != nil {
		return err
	}

	return nil
}
