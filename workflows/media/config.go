// Package media provides the Temporal workflow definition for processing media
// files (movies and TV episodes) using a single parameterised workflow.
package media

import (
	"fmt"
	"time"

	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
)

// MediaWorkflowName is the registered Temporal workflow name. Re-exported from
// the types package for callers that import workflows/media directly.
const MediaWorkflowName = mediatypes.MediaWorkflowName

// Registered activity names. Workflows reference activities by these strings
// so registration and invocation cannot drift from one another. Each name maps
// 1:1 to a method on Activities; the operational boundary (idempotency, retry
// policy, data inputs) is what determines the split, so each activity owns
// exactly one concern and each concern is invoked under the retry policy that
// fits it.
const (
	ProbeActivityName         = "Probe"
	DetectCropActivityName    = "DetectCrop"
	TranscodeActivityName     = "Transcode"
	NotifyActivityName        = "Notify"
	CleanupActivityName       = "Cleanup"
	NotifyFailureActivityName = "NotifyFailure"
)

// Activity tokens: kebab-case identifiers used in the worker's activity
// selection list (WORKER_ACTIVITIES) and as the suffix on each activity's
// dedicated task queue. The mapping is documented in docs/configuration.md.
const (
	ProbeActivityToken         = "probe"
	DetectCropActivityToken    = "detect-crop"
	TranscodeActivityToken     = "transcode"
	NotifyActivityToken        = "notify"
	CleanupActivityToken       = "cleanup"
	NotifyFailureActivityToken = "notify-failure"

	// WorkflowToken is the WORKER_ACTIVITIES token that enables a worker's
	// workflow task-queue Worker. Unlike activity tokens, the workflow worker
	// polls the prefix-only queue (no suffix); see ActivityTaskQueue.
	WorkflowToken = "workflow"
)

// DefaultTaskQueuePrefix is the prefix applied to the workflow task queue and
// every activity task queue. Operators can override it via TEMPORAL_TASK_QUEUE
// (the watcher dispatches workflows there, the worker polls there); tests pass
// a per-test value through MediaWorkflowConfig.TaskQueuePrefix to keep parallel
// runs from sharing queues. Re-exported from the types package so the watcher
// can read it without pulling in libav.
const DefaultTaskQueuePrefix = mediatypes.DefaultTaskQueuePrefix

// KnownActivities lists every activity's kebab-case token in the order the
// workflow invokes them. WORKER_ACTIVITIES references these tokens; the worker
// uses this list to register one Temporal Worker per resolved activity.
var KnownActivities = []string{
	ProbeActivityToken,
	DetectCropActivityToken,
	TranscodeActivityToken,
	NotifyActivityToken,
	CleanupActivityToken,
	NotifyFailureActivityToken,
}

// ActivityTokensByName maps each Temporal-registered activity name to its
// kebab-case token. The workflow uses this to derive ActivityOptions.TaskQueue
// per ExecuteActivity call.
var ActivityTokensByName = map[string]string{
	ProbeActivityName:         ProbeActivityToken,
	DetectCropActivityName:    DetectCropActivityToken,
	TranscodeActivityName:     TranscodeActivityToken,
	NotifyActivityName:        NotifyActivityToken,
	CleanupActivityName:       CleanupActivityToken,
	NotifyFailureActivityName: NotifyFailureActivityToken,
}

// ActivityTaskQueue returns the task queue name for the activity identified by
// its kebab-case token. The result is "{prefix}-{token}", e.g.
// "media-processor-detect-crop".
func ActivityTaskQueue(prefix, token string) string {
	return prefix + "-" + token
}

// ActivityTaskQueueByName is a convenience for workflow code that already has
// the Temporal activity name (e.g. DetectCropActivityName) and wants the task
// queue to route to. Panics if activityName is not registered in
// ActivityTokensByName — that would silently route to "{prefix}-" and strand
// activity tasks, so a programming error here must fail loud at the call site.
func ActivityTaskQueueByName(prefix, activityName string) string {
	token, ok := ActivityTokensByName[activityName]
	if !ok {
		panic(fmt.Sprintf("ActivityTaskQueueByName: unknown activity name %q", activityName))
	}

	return ActivityTaskQueue(prefix, token)
}

const (
	// DefaultDetectCropTimeout is the default Temporal StartToCloseTimeout for
	// the detectcrop activity, used when MediaWorkflowConfig.DetectCropTimeout
	// is zero.
	DefaultDetectCropTimeout = 30 * time.Minute
	// DefaultTranscodeTimeout is the default Temporal StartToCloseTimeout for
	// the transcode activity, used when MediaWorkflowConfig.TranscodeTimeout is
	// zero.
	DefaultTranscodeTimeout = 4 * time.Hour

	// defaultProbeTimeout is the StartToCloseTimeout applied to the probe activity.
	defaultProbeTimeout = 5 * time.Minute
	// defaultFinalizeTimeout is the StartToCloseTimeout applied to cleanup and
	// the failure-webhook activity.
	defaultFinalizeTimeout = 10 * time.Minute
	// defaultNotifyTimeout is the StartToCloseTimeout applied to the notify
	// activity. Notify blocks on Sonarr/Radarr command completion, and those
	// services run import-disk commands strictly serially across a 3-thread
	// pool, so a queued scan can wait behind many siblings before executing.
	// One hour covers ~120 serialized scans at a generous 30s each.
	defaultNotifyTimeout = 1 * time.Hour

	// defaultMaxAttempts is the RetryPolicy MaximumAttempts applied to probe,
	// detectcrop, transcode, and the failure-webhook. Single attempt: retry
	// would either repeat expensive work that will not recover (probe /
	// detectcrop / transcode) or duplicate a non-idempotent side effect
	// (webhook).
	defaultMaxAttempts = 1
	// cleanupMaxAttempts is the RetryPolicy MaximumAttempts applied to the
	// cleanup activity. RunCleanup tolerates ErrNotExist so retries are safe
	// when the filesystem is transiently unavailable.
	cleanupMaxAttempts = 3
)

// Defaults for the Notify activity's retry policy. Notify is idempotent
// (re-issuing a Sonarr/Radarr scan for an already-imported file is a no-op)
// and the most common failure mode is the arr service not yet seeing the
// transcoded file because of NFS attribute-cache staleness on its side. The
// schedule (5s, 7.5s, 11s, 17s, 25s, 38s, 57s, 60s × 7 = ~9min wall time)
// retries quickly to clear transient command-queue saturation while still
// extending past a typical 60s NFS acdirmax window.
const (
	DefaultNotifyInitialInterval    = 5 * time.Second
	DefaultNotifyBackoffCoefficient = 1.5
	DefaultNotifyMaximumInterval    = 60 * time.Second
	DefaultNotifyMaximumAttempts    = int32(15)
)

// MediaWorkflowConfig holds the configuration for the media processing workflow
// and its activities.
type MediaWorkflowConfig struct {
	// TaskQueuePrefix is the base task queue name. The workflow itself runs on
	// this exact queue; each activity is routed to "{prefix}-{token}" via
	// ActivityOptions.TaskQueue. When zero-value, NewActivities applies
	// DefaultTaskQueuePrefix ("media-processor"). Tests override this so
	// parallel runs do not share activity queues.
	TaskQueuePrefix string
	// HighCardinalityLabels controls whether per-item labels (id, title, year, etc.)
	// are attached to metric observations. Corresponds to METRICS_HIGH_CARDINALITY_LABELS.
	HighCardinalityLabels bool
	// MinCropX is the minimum number of pixels that must be trimmed horizontally
	// for a crop to be applied. -1 disables the threshold (any crop is accepted).
	// 0 means no minimum (any detected crop is applied).
	MinCropX int
	// MinCropY is the minimum number of pixels that must be trimmed vertically
	// for a crop to be applied. -1 disables the threshold (any crop is accepted).
	// 0 means no minimum (any detected crop is applied).
	MinCropY int
	// DetectCropTimeout is the Temporal StartToCloseTimeout for the detectcrop
	// activity. When zero, NewActivities applies a default of 30 minutes.
	DetectCropTimeout time.Duration
	// TranscodeTimeout is the Temporal StartToCloseTimeout for the transcode
	// activity. When zero, NewActivities applies a default of 4 hours.
	TranscodeTimeout time.Duration
	// H265CRF is the constant-quality value passed to H.265 encoders. 0 means
	// use the encoder's built-in default.
	H265CRF int
	// ProgressLogInterval controls how often a progress log line is emitted
	// during transcoding. Zero disables progress logging.
	ProgressLogInterval time.Duration
	// NotifyInitialInterval is the RetryPolicy InitialInterval for the Notify
	// activity. When zero, NewActivities applies DefaultNotifyInitialInterval.
	NotifyInitialInterval time.Duration
	// NotifyBackoffCoefficient is the RetryPolicy BackoffCoefficient for the
	// Notify activity. When zero, NewActivities applies
	// DefaultNotifyBackoffCoefficient.
	NotifyBackoffCoefficient float64
	// NotifyMaximumInterval is the RetryPolicy MaximumInterval for the Notify
	// activity. When zero, NewActivities applies DefaultNotifyMaximumInterval.
	NotifyMaximumInterval time.Duration
	// NotifyMaximumAttempts is the RetryPolicy MaximumAttempts for the Notify
	// activity. When zero, NewActivities applies DefaultNotifyMaximumAttempts.
	NotifyMaximumAttempts int32
}

// MediaInput is an alias for the shared input type so existing callers within
// this package do not need to be updated.
type MediaInput = mediatypes.MediaInput

// transcodeHeartbeatTimeout returns the Temporal HeartbeatTimeout to apply to
// the transcode activity given the configured progress-log interval. The
// heartbeat goroutine emits a heartbeat on every FFmpeg progress tick, so the
// timeout is set to twice the log interval to absorb the natural jitter
// between ticks (a single missed tick must not fail the activity).
//
// When the progress interval is zero (operator disabled progress logging),
// this returns zero, which Temporal treats as "no heartbeat enforcement". An
// operator who opts out of progress signalling also opts out of heartbeat-
// based stall detection.
func transcodeHeartbeatTimeout(progressLogInterval time.Duration) time.Duration {
	if progressLogInterval <= 0 {
		return 0
	}

	return 2 * progressLogInterval
}
