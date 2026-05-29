package media

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// activityOptions builds the (TaskQueue, StartToCloseTimeout, RetryPolicy)
// triple used by every activity in the workflow. The activity name selects
// the dedicated task queue via ActivityTaskQueueByName so a worker pod that
// only polls the matching queue picks up the call. The transcode activity
// additionally sets HeartbeatTimeout on the returned struct.
func (a *Activities) activityOptions(activityName string, timeout time.Duration, attempts int32) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           ActivityTaskQueueByName(a.cfg.TaskQueuePrefix, activityName),
		StartToCloseTimeout: timeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: attempts},
	}
}

// notifyActivityOptions builds the ActivityOptions for the Notify activity.
// Notify uses a richer RetryPolicy than the other activities: the typical
// failure mode is the arr service not yet seeing the transcoded file because
// of NFS attribute-cache staleness on its side, so the policy retries
// frequently at first (covering arr command-queue saturation) and grows out
// past the typical NFS acdirmax window. All four knobs are tunable via env
// vars on the worker; defaults live in DefaultNotify* constants in this
// package.
func (a *Activities) notifyActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           ActivityTaskQueueByName(a.cfg.TaskQueuePrefix, NotifyActivityName),
		StartToCloseTimeout: defaultNotifyTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    a.cfg.NotifyInitialInterval,
			BackoffCoefficient: a.cfg.NotifyBackoffCoefficient,
			MaximumInterval:    a.cfg.NotifyMaximumInterval,
			MaximumAttempts:    a.cfg.NotifyMaximumAttempts,
		},
	}
}

// MediaWorkflow processes one media file end-to-end. The body is straight-line
// Go: probe, branch on validity, run the per-path activities. A defer block
// invokes the failure-webhook activity when the workflow returns an error.
//
// Each activity is invoked with the retry policy that fits its idempotency
// profile: Notify uses a configurable multi-attempt policy (see
// notifyActivityOptions) tuned for NFS-cache stalls on the arr side; Cleanup
// uses cleanupMaxAttempts so transient filesystem flakes do not fail the run;
// everything else (Probe, DetectCrop, Transcode, NotifyFailure) uses
// defaultMaxAttempts (single attempt) because retrying would either duplicate
// non-idempotent side effects or repeat expensive work that will not recover.
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

		failureCtx := workflow.WithActivityOptions(disconnected, a.activityOptions(NotifyFailureActivityName, defaultFinalizeTimeout, defaultMaxAttempts))

		notifyErr := workflow.ExecuteActivity(failureCtx, NotifyFailureActivityName, input, step, message).Get(failureCtx, nil)
		if notifyErr != nil {
			log.Error("failure-webhook activity failed", "error", notifyErr.Error())
		}
	}()

	probeCtx := workflow.WithActivityOptions(ctx, a.activityOptions(ProbeActivityName, defaultProbeTimeout, defaultMaxAttempts))

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
		invalidCleanupCtx := workflow.WithActivityOptions(ctx, a.activityOptions(CleanupActivityName, defaultFinalizeTimeout, cleanupMaxAttempts))

		if err := workflow.ExecuteActivity(invalidCleanupCtx, CleanupActivityName, input, TranscodeOutput{}, NotifyOutput{}).Get(invalidCleanupCtx, nil); err != nil {
			return err
		}

		return nil
	}

	var crop DetectCropOutput

	// When crop detection is disabled for this watch, skip the detect-crop
	// activity entirely. A nil crop means no crop filter is applied (the full
	// frame is transcoded), so no detect-crop worker is needed for these files.
	if input.SkipCropDetection {
		log.Info("crop detection skipped (disabled for watch)", "file", input.FilePath)
	} else {
		cropCtx := workflow.WithActivityOptions(ctx, a.activityOptions(DetectCropActivityName, a.cfg.DetectCropTimeout, defaultMaxAttempts))

		if err := workflow.ExecuteActivity(cropCtx, DetectCropActivityName, input, probe).Get(cropCtx, &crop); err != nil {
			return err
		}
	}

	transcodeOpts := a.activityOptions(TranscodeActivityName, a.cfg.TranscodeTimeout, defaultMaxAttempts)
	transcodeOpts.HeartbeatTimeout = transcodeHeartbeatTimeout(a.cfg.ProgressLogInterval)
	transcodeCtx := workflow.WithActivityOptions(ctx, transcodeOpts)

	var transcode TranscodeOutput
	if err := workflow.ExecuteActivity(transcodeCtx, TranscodeActivityName, input, probe, crop).Get(transcodeCtx, &transcode); err != nil {
		return err
	}

	notifyCtx := workflow.WithActivityOptions(ctx, a.notifyActivityOptions())

	var notify NotifyOutput
	if err := workflow.ExecuteActivity(notifyCtx, NotifyActivityName, input, transcode).Get(notifyCtx, &notify); err != nil {
		return err
	}

	cleanupCtx := workflow.WithActivityOptions(ctx, a.activityOptions(CleanupActivityName, defaultFinalizeTimeout, cleanupMaxAttempts))

	if err := workflow.ExecuteActivity(cleanupCtx, CleanupActivityName, input, transcode, notify).Get(cleanupCtx, nil); err != nil {
		return err
	}

	return nil
}
