// Package media provides the Temporal workflow definition for processing media
// files (movies and TV episodes) using a single parameterised workflow.
package media

import (
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
	// retryableMaxAttempts is the RetryPolicy MaximumAttempts applied to the
	// notify and cleanup activities. Both are idempotent — the arr scan
	// command is a no-op when re-issued for an already-imported file, and
	// RunCleanup tolerates ErrNotExist — so retries are safe and useful when
	// the arr service or filesystem is transiently unavailable.
	retryableMaxAttempts = 3
)

// MediaWorkflowConfig holds the configuration for the media processing workflow
// and its activities.
type MediaWorkflowConfig struct {
	// HardwareDevicePath is the device path passed to CreateHardwareDeviceContext
	// for hardware-accelerated transcoding. An empty string uses libav auto-select.
	HardwareDevicePath string
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
