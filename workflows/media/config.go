// Package media provides the Temporal workflow definition for processing media
// files (movies and TV episodes) using a single parameterised workflow.
package media

import (
	"time"

	otelmetric "go.opentelemetry.io/otel/metric"

	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
	"github.com/solidDoWant/media-processor/workflows/steps"
)

// MediaWorkflowName is the registered Temporal workflow name. Re-exported from
// the types package for callers that import workflows/media directly.
const MediaWorkflowName = mediatypes.MediaWorkflowName

// Registered activity names. Workflows reference activities by these strings
// so registration and invocation cannot drift from one another.
const (
	ProbeActivityName      = "Probe"
	DetectCropActivityName = "DetectCrop"
	TranscodeActivityName  = "Transcode"
	FinalizeActivityName   = "Finalize"
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
	// defaultFinalizeTimeout is the StartToCloseTimeout applied to the finalize
	// activity in all three modes (valid / invalid / failure).
	defaultFinalizeTimeout = 10 * time.Minute

	// defaultMaxAttempts is the RetryPolicy MaximumAttempts applied to probe,
	// detectcrop, transcode, the metrics finalize sub-mode, the invalid path,
	// and the failure-webhook. Single attempt: retry would either repeat
	// expensive work that will not recover (probe / detectcrop / transcode) or
	// duplicate a non-idempotent side effect (metrics emission, webhook).
	defaultMaxAttempts = 1
	// finalizeRetryableMaxAttempts is the RetryPolicy MaximumAttempts applied
	// to the notify and cleanup finalize sub-modes. Both are idempotent — the
	// arr scan command is a no-op when re-issued for an already-imported file,
	// and RunCleanup tolerates ErrNotExist — so retries are safe and useful
	// when the arr service or filesystem is transiently unavailable.
	finalizeRetryableMaxAttempts = 3
)

// MediaInput is an alias for the shared input type so existing callers within
// this package do not need to be updated.
type MediaInput = mediatypes.MediaInput

// MediaWorkflowConfig holds the configuration for the media processing workflow
// and its activities.
type MediaWorkflowConfig struct {
	// HardwareDevicePath is the device path passed to CreateHardwareDeviceContext
	// for hardware-accelerated transcoding. An empty string uses libav auto-select.
	HardwareDevicePath string
	// MeterProvider is the OTel MeterProvider used for per-run metrics. When nil,
	// a no-op provider is used and no metrics are emitted.
	MeterProvider otelmetric.MeterProvider
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

// FinalizeMode discriminates the branches of the Finalize activity. The valid
// path is split into three sub-modes (Notify, Cleanup, Metrics) so the
// workflow can give each sub-step the retry policy that matches its
// idempotency profile, instead of coupling all three to one shared retry
// budget. The invalid path stays as a single non-retryable mode because
// invalid files are rare and the cleanup-then-metrics window is tiny.
// Folding everything into one registered activity keeps the workflow at four
// registered activities total.
type FinalizeMode int

const (
	// FinalizeNotify performs the library import (Sonarr/Radarr scan command).
	// Idempotent in practice, so the workflow retries this mode on transient
	// failures.
	FinalizeNotify FinalizeMode = iota + 1
	// FinalizeCleanup deletes the source file or writes the .done sentinel.
	// Idempotent (RunCleanup tolerates ErrNotExist), so the workflow retries
	// this mode on transient failures.
	FinalizeCleanup
	// FinalizeMetrics records per-run histograms and the optional GetInfo-
	// driven counters. Histograms are not idempotent — every Record() emits a
	// fresh observation — so the workflow invokes this mode without retries
	// and ignores its error.
	FinalizeMetrics
	// FinalizeInvalid runs the cleanup-and-metrics path for a file that the
	// probe activity determined was not valid media. Single attempt.
	FinalizeInvalid
	// FinalizeFailure runs from the workflow's defer block to send a single
	// aggregated failure notification when the workflow returns an error.
	FinalizeFailure
)

// FinalizeInput is the activity payload for Finalize. Only the fields relevant
// to the chosen Mode are read.
type FinalizeInput struct {
	Mode      FinalizeMode
	Input     MediaInput
	Probe     steps.ProbeOutput     // valid + invalid modes
	Transcode steps.TranscodeOutput // valid mode

	// FailureStep is the activity name where the workflow error originated.
	// Only set when Mode == FinalizeFailure.
	FailureStep string
	// FailureErr is the error message from the failed activity. Only set when
	// Mode == FinalizeFailure.
	FailureErr string
}
