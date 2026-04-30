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
	// detectcrop, and transcode: these activities are not retried because their
	// failure modes (corrupt input, missing crop region, ffmpeg crash) generally
	// will not recover on a retry.
	defaultMaxAttempts = 1
	// finalizeValidMaxAttempts is the RetryPolicy MaximumAttempts applied to
	// the valid-path Finalize invocation. Library import and source cleanup are
	// idempotent and benefit from retries when the arr service or filesystem is
	// transiently unavailable.
	finalizeValidMaxAttempts = 3
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

// FinalizeMode discriminates the three branches of the Finalize activity.
// Folding three short side-effecting tasks (record_metrics, record_invalid,
// finalize/cleanup) plus the failure-webhook into one activity keeps the
// workflow at four registered activities total.
type FinalizeMode int

const (
	// FinalizeValid runs the post-transcode work for a successfully processed
	// file: library import, source cleanup or sentinel, and per-run metrics.
	FinalizeValid FinalizeMode = iota + 1
	// FinalizeInvalid runs the cleanup-and-metrics path for a file that the
	// probe activity determined was not valid media.
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
