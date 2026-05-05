package main

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/solidDoWant/media-processor/internal/envvar"
	"github.com/solidDoWant/media-processor/pkg/medialib/radarr"
	"github.com/solidDoWant/media-processor/pkg/medialib/sonarr"
	"github.com/solidDoWant/media-processor/pkg/transcodelimiter"
	"github.com/solidDoWant/media-processor/workflows/media"
)

// workerConfig bundles the per-subsystem configuration consumed by run().
// Concentrating env-var reads here keeps main.go to wiring only and gives
// operators a single file to grep when answering "what does this binary read?".
type workerConfig struct {
	LogLevel string
	// TaskQueuePrefix is the workflow task queue (set from TEMPORAL_TASK_QUEUE,
	// default media.DefaultTaskQueuePrefix). Activity queues are derived from
	// it via media.ActivityTaskQueue.
	TaskQueuePrefix string
	// EnabledTokens is the resolved WORKER_ACTIVITIES set. Each entry is
	// either media.WorkflowToken or one of media.KnownActivities.
	EnabledTokens     []string
	HealthAddr        string
	WebhookURL        string
	WorkerStopTimeout time.Duration
	// HardwareDevicePathOverride is the operator-supplied
	// MEDIA_HARDWARE_DEVICE_PATH value (empty when unset). When non-empty it
	// overrides hardware auto-detection and is validated at startup;
	// auto-detection of an Intel i915 render node runs only when this is
	// empty (and only on workers with the transcode activity enabled).
	HardwareDevicePathOverride string
	Radarr                     radarr.Config
	Sonarr                     sonarr.Config
	Workflow                   media.MediaWorkflowConfig
	// TranscodeLimiter holds the slot-supplier tuning for transcode-enabled
	// workers. Read from MEDIA_TRANSCODE_LIMITER_* env vars; the values are
	// only consulted when the worker actually polls the transcode queue.
	TranscodeLimiter transcodeLimiterConfig
}

// transcodeLimiterConfig bundles the operator-tunable parameters for the
// transcode admission controller. Mirrors transcodelimiter.Config plus the
// sampler-side knobs (interval, smoothing window) so a single struct
// describes the full surface area.
type transcodeLimiterConfig struct {
	Limiter         transcodelimiter.Config
	SampleInterval  time.Duration
	SmoothingWindow int
}

// Defaults for the transcode limiter, mirrored in docs/configuration.md.
const (
	defaultLimiterStaticCap             = 5
	defaultLimiterGPUThreshold          = 0.8
	defaultLimiterPostAdmissionCooldown = 3 * time.Second
	defaultLimiterSampleInterval        = 500 * time.Millisecond
	defaultLimiterSmoothingWindow       = 5
)

// defaultHealthAddr is the fallback HTTP listen address for the health server
// when HEALTH_ADDR is unset.
const defaultHealthAddr = ":8080"

// loadConfig reads worker configuration from environment variables and returns
// a bundle of sub-structs ready to be handed to existing constructors.
//
// METRICS_ADDR is intentionally not read here — metrics.NewFromEnv owns it.
func loadConfig() (workerConfig, error) {
	cfg := workerConfig{
		LogLevel:   os.Getenv("LOG_LEVEL"),
		HealthAddr: os.Getenv("HEALTH_ADDR"),
		WebhookURL: os.Getenv("MEDIA_WEBHOOK_URL"),
	}

	if cfg.HealthAddr == "" {
		cfg.HealthAddr = defaultHealthAddr
	}

	cfg.TaskQueuePrefix = os.Getenv("TEMPORAL_TASK_QUEUE")
	if cfg.TaskQueuePrefix == "" {
		cfg.TaskQueuePrefix = media.DefaultTaskQueuePrefix
	}

	known := append([]string{media.WorkflowToken}, media.KnownActivities...)

	enabledTokens, err := resolveActivities(parseWorkerActivities(os.Getenv("WORKER_ACTIVITIES")), known)
	if err != nil {
		return workerConfig{}, err
	}

	cfg.EnabledTokens = enabledTokens

	radarrURL, err := envvar.RequireEnv("RADARR_URL")
	if err != nil {
		return workerConfig{}, err
	}

	radarrAPIKey, err := envvar.RequireEnv("RADARR_API_KEY")
	if err != nil {
		return workerConfig{}, err
	}

	cfg.Radarr = radarr.Config{URL: radarrURL, APIKey: radarrAPIKey}

	sonarrURL, err := envvar.RequireEnv("SONARR_URL")
	if err != nil {
		return workerConfig{}, err
	}

	sonarrAPIKey, err := envvar.RequireEnv("SONARR_API_KEY")
	if err != nil {
		return workerConfig{}, err
	}

	cfg.Sonarr = sonarr.Config{URL: sonarrURL, APIKey: sonarrAPIKey}

	hardwareDevicePathOverride := os.Getenv("MEDIA_HARDWARE_DEVICE_PATH")

	// Only enforce the override when this worker actually transcodes. With
	// shared config across worker controllers, a GPU path set for the
	// transcode worker must not fail startup on CPU-only workers that never
	// open the device.
	if slices.Contains(cfg.EnabledTokens, media.TranscodeActivityToken) {
		if err := validateHardwareDevicePath(hardwareDevicePathOverride); err != nil {
			return workerConfig{}, err
		}
	}

	cfg.HardwareDevicePathOverride = hardwareDevicePathOverride

	workflow, err := loadWorkflowConfig()
	if err != nil {
		return workerConfig{}, err
	}

	workflow.TaskQueuePrefix = cfg.TaskQueuePrefix
	cfg.Workflow = workflow

	// Default WORKER_STOP_TIMEOUT to the effective transcodeTimeout (not
	// media.DefaultTranscodeTimeout) so an operator who raises
	// MEDIA_TRANSCODE_TIMEOUT does not also have to set WORKER_STOP_TIMEOUT to
	// keep the drain ceiling above the longest activity.
	workerStopTimeout, err := parseTimeout("WORKER_STOP_TIMEOUT", workflow.TranscodeTimeout)
	if err != nil {
		return workerConfig{}, err
	}

	cfg.WorkerStopTimeout = workerStopTimeout

	limiter, err := loadTranscodeLimiterConfig()
	if err != nil {
		return workerConfig{}, err
	}

	cfg.TranscodeLimiter = limiter

	return cfg, nil
}

// loadTranscodeLimiterConfig reads the five MEDIA_TRANSCODE_LIMITER_* env vars
// and applies the documented defaults when a variable is unset. Each override
// is logged so operators can confirm the values that took effect; the values
// are only consulted when the worker actually polls the transcode queue, so
// non-transcode workers pay no attention to them.
func loadTranscodeLimiterConfig() (transcodeLimiterConfig, error) {
	staticCap, err := parsePositiveInt("MEDIA_TRANSCODE_LIMITER_STATIC_CAP", defaultLimiterStaticCap)
	if err != nil {
		return transcodeLimiterConfig{}, err
	}

	gpuThreshold, err := parseUnitFloat("MEDIA_TRANSCODE_LIMITER_GPU_THRESHOLD", defaultLimiterGPUThreshold)
	if err != nil {
		return transcodeLimiterConfig{}, err
	}

	cooldown, err := parseTimeout("MEDIA_TRANSCODE_LIMITER_POST_ADMISSION_COOLDOWN", defaultLimiterPostAdmissionCooldown)
	if err != nil {
		return transcodeLimiterConfig{}, err
	}

	sampleInterval, err := parseTimeout("MEDIA_TRANSCODE_LIMITER_SAMPLE_INTERVAL", defaultLimiterSampleInterval)
	if err != nil {
		return transcodeLimiterConfig{}, err
	}

	smoothingWindow, err := parsePositiveInt("MEDIA_TRANSCODE_LIMITER_SMOOTHING_WINDOW", defaultLimiterSmoothingWindow)
	if err != nil {
		return transcodeLimiterConfig{}, err
	}

	logLimiterOverride("MEDIA_TRANSCODE_LIMITER_STATIC_CAP", staticCap, defaultLimiterStaticCap)
	logLimiterOverride("MEDIA_TRANSCODE_LIMITER_GPU_THRESHOLD", gpuThreshold, defaultLimiterGPUThreshold)
	logLimiterOverride("MEDIA_TRANSCODE_LIMITER_POST_ADMISSION_COOLDOWN", cooldown, defaultLimiterPostAdmissionCooldown)
	logLimiterOverride("MEDIA_TRANSCODE_LIMITER_SAMPLE_INTERVAL", sampleInterval, defaultLimiterSampleInterval)
	logLimiterOverride("MEDIA_TRANSCODE_LIMITER_SMOOTHING_WINDOW", smoothingWindow, defaultLimiterSmoothingWindow)

	return transcodeLimiterConfig{
		Limiter: transcodelimiter.Config{
			StaticCap:             staticCap,
			GPUThreshold:          gpuThreshold,
			PostAdmissionCooldown: cooldown,
		},
		SampleInterval:  sampleInterval,
		SmoothingWindow: smoothingWindow,
	}, nil
}

// logLimiterOverride emits an info log line when the resolved value differs
// from the documented default. The comparison uses the comparable interface
// so the same helper handles ints, durations, and floats.
func logLimiterOverride[T comparable](envVar string, value, defaultValue T) {
	if value == defaultValue {
		return
	}

	slog.Info("transcode limiter override",
		slog.String("env_var", envVar),
		slog.Any("value", value),
		slog.Any("default", defaultValue),
	)
}

// parsePositiveInt reads a positive integer from the named env var. Empty
// input returns defaultVal; non-integer or non-positive values are a fatal
// error naming the var and the offending value.
func parsePositiveInt(envVar string, defaultVal int) (int, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return defaultVal, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive integer (got %q): %w", envVar, raw, err)
	}

	if v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer (got %q)", envVar, raw)
	}

	return v, nil
}

// parseUnitFloat reads a float in the half-open interval (0, 1] from the
// named env var. Empty input returns defaultVal; out-of-range or non-numeric
// values are a fatal error naming the var and the offending value.
func parseUnitFloat(envVar string, defaultVal float64) (float64, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return defaultVal, nil
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a float in (0, 1] (got %q): %w", envVar, raw, err)
	}

	if v <= 0 || v > 1 {
		return 0, fmt.Errorf("%s must be a float in (0, 1] (got %q)", envVar, raw)
	}

	return v, nil
}

// loadWorkflowConfig reads the media-workflow tuning env vars and returns a
// ready-to-use MediaWorkflowConfig. Split from loadConfig to keep the dense
// crop/timeout/CRF block testable in isolation.
func loadWorkflowConfig() (media.MediaWorkflowConfig, error) {
	minCropX, err := parseCropThreshold("MEDIA_MIN_CROP_X", 10)
	if err != nil {
		return media.MediaWorkflowConfig{}, err
	}

	minCropY, err := parseCropThreshold("MEDIA_MIN_CROP_Y", 10)
	if err != nil {
		return media.MediaWorkflowConfig{}, err
	}

	detectCropTimeout, err := parseTimeout("MEDIA_DETECT_CROP_TIMEOUT", media.DefaultDetectCropTimeout)
	if err != nil {
		return media.MediaWorkflowConfig{}, err
	}

	transcodeTimeout, err := parseTimeout("MEDIA_TRANSCODE_TIMEOUT", media.DefaultTranscodeTimeout)
	if err != nil {
		return media.MediaWorkflowConfig{}, err
	}

	h265CRF, err := parseH265CRF("MEDIA_H265_CRF")
	if err != nil {
		return media.MediaWorkflowConfig{}, err
	}

	progressLogInterval, err := parseTimeout("MEDIA_PROGRESS_LOG_INTERVAL", 30*time.Second)
	if err != nil {
		return media.MediaWorkflowConfig{}, err
	}

	highCardinalityLabels, err := envvar.ParseBool("METRICS_HIGH_CARDINALITY_LABELS", false)
	if err != nil {
		return media.MediaWorkflowConfig{}, err
	}

	return media.MediaWorkflowConfig{
		HighCardinalityLabels: highCardinalityLabels,
		MinCropX:              minCropX,
		MinCropY:              minCropY,
		DetectCropTimeout:     detectCropTimeout,
		TranscodeTimeout:      transcodeTimeout,
		H265CRF:               h265CRF,
		ProgressLogInterval:   progressLogInterval,
	}, nil
}

// parseCropThreshold reads an integer from the named environment variable.
// If the variable is unset or empty, defaultVal is returned. A value of -1 is
// accepted (disables the threshold). Any other non-integer value is a fatal error.
func parseCropThreshold(envVar string, defaultVal int) (int, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return defaultVal, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer (got %q): %w", envVar, raw, err)
	}

	return v, nil
}

// parseH265CRF reads the H.265 constant-quality value from the named environment
// variable. If the variable is unset or empty, 0 is returned (encoder default).
// Valid values are 1–51; any other non-integer or out-of-range value is a fatal error.
func parseH265CRF(envVar string) (int, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return 0, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 || v > 51 {
		return 0, fmt.Errorf("%s must be an integer between 1 and 51 (got %q)", envVar, raw)
	}

	return v, nil
}

// parseTimeout reads a Go duration string from the named environment variable.
// If the variable is unset or empty, defaultVal is returned. Any non-duration
// value is a fatal error.
func parseTimeout(envVar string, defaultVal time.Duration) (time.Duration, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return defaultVal, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration (got %q): %w", envVar, raw, err)
	}

	return d, nil
}

// validateHardwareDevicePath rejects paths that exist but are not character
// devices and paths that do not exist at all, surfacing typos like
// "/dev/dri/render128" (missing D) before any workflow is dispatched.
func validateHardwareDevicePath(path string) error {
	if path == "" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("MEDIA_HARDWARE_DEVICE_PATH %q: %w", path, err)
	}

	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("MEDIA_HARDWARE_DEVICE_PATH %q is not a character device", path)
	}

	return nil
}
