package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/media-processor/internal/envvar"
	"github.com/solidDoWant/media-processor/pkg/health"
	"github.com/solidDoWant/media-processor/pkg/logging"
	"github.com/solidDoWant/media-processor/pkg/medialib/radarr"
	"github.com/solidDoWant/media-processor/pkg/medialib/sonarr"
	"github.com/solidDoWant/media-processor/pkg/metrics"
	"github.com/solidDoWant/media-processor/pkg/temporalclient"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows/media"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, worker.InterruptCh()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, interruptCh <-chan interface{}) error {
	logging.Setup(os.Getenv("LOG_LEVEL"))

	taskQueue, err := envvar.RequireEnv("TEMPORAL_TASK_QUEUE")
	if err != nil {
		return err
	}

	const defaultHealthAddr = ":8080"

	healthAddr := os.Getenv("HEALTH_ADDR")
	if healthAddr == "" {
		healthAddr = defaultHealthAddr
	}

	healthServer, err := health.New(ctx, healthAddr)
	if err != nil {
		return fmt.Errorf("init health server: %w", err)
	}

	// Default to :9090 (paired with the worker's health port at :8080) so
	// running the watcher and worker side-by-side on the same host doesn't
	// collide on the metrics port. METRICS_ADDR overrides this.
	const defaultMetricsAddr = ":9090"

	metricsProvider, shutdown, err := metrics.NewFromEnv(defaultMetricsAddr)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer shutdown()

	radarrURL, err := envvar.RequireEnv("RADARR_URL")
	if err != nil {
		return err
	}

	radarrAPIKey, err := envvar.RequireEnv("RADARR_API_KEY")
	if err != nil {
		return err
	}

	sonarrURL, err := envvar.RequireEnv("SONARR_URL")
	if err != nil {
		return err
	}

	sonarrAPIKey, err := envvar.RequireEnv("SONARR_API_KEY")
	if err != nil {
		return err
	}

	radarrClient := radarr.New(radarr.Config{
		URL:    radarrURL,
		APIKey: radarrAPIKey,
	})

	sonarrClient := sonarr.New(sonarr.Config{
		URL:    sonarrURL,
		APIKey: sonarrAPIKey,
	})

	webhookClient := &webhook.Client{
		URL: os.Getenv("MEDIA_WEBHOOK_URL"),
	}

	minCropX, err := parseCropThreshold("MEDIA_MIN_CROP_X", 10)
	if err != nil {
		return err
	}

	minCropY, err := parseCropThreshold("MEDIA_MIN_CROP_Y", 10)
	if err != nil {
		return err
	}

	detectCropTimeout, err := parseTimeout("MEDIA_DETECT_CROP_TIMEOUT", media.DefaultDetectCropTimeout)
	if err != nil {
		return err
	}

	transcodeTimeout, err := parseTimeout("MEDIA_TRANSCODE_TIMEOUT", media.DefaultTranscodeTimeout)
	if err != nil {
		return err
	}

	h265CRF, err := parseH265CRF("MEDIA_H265_CRF")
	if err != nil {
		return err
	}

	progressLogInterval, err := parseTimeout("MEDIA_PROGRESS_LOG_INTERVAL", 30*time.Second)
	if err != nil {
		return err
	}

	// Default to the effective transcodeTimeout (not media.DefaultTranscodeTimeout)
	// so an operator who raises MEDIA_TRANSCODE_TIMEOUT does not also have to set
	// WORKER_STOP_TIMEOUT to keep the drain ceiling above the longest activity.
	workerStopTimeout, err := parseTimeout("WORKER_STOP_TIMEOUT", transcodeTimeout)
	if err != nil {
		return err
	}

	highCardinalityLabels, err := envvar.ParseBool("METRICS_HIGH_CARDINALITY_LABELS", false)
	if err != nil {
		return err
	}

	hardwareDevicePath := os.Getenv("MEDIA_HARDWARE_DEVICE_PATH")
	if err := validateHardwareDevicePath(hardwareDevicePath); err != nil {
		return err
	}

	activities, err := media.NewActivities(media.MediaWorkflowConfig{
		HardwareDevicePath:    hardwareDevicePath,
		HighCardinalityLabels: highCardinalityLabels,
		MinCropX:              minCropX,
		MinCropY:              minCropY,
		DetectCropTimeout:     detectCropTimeout,
		TranscodeTimeout:      transcodeTimeout,
		H265CRF:               h265CRF,
		ProgressLogInterval:   progressLogInterval,
	}, radarrClient, sonarrClient, webhookClient)
	if err != nil {
		return fmt.Errorf("init activities: %w", err)
	}

	temporalClient, shutdownTemporal, err := temporalclient.Dial(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		return err
	}
	defer shutdownTemporal()

	w := worker.New(temporalClient, taskQueue, worker.Options{
		WorkerStopTimeout: workerStopTimeout,
	})

	activities.Register(w)

	slog.InfoContext(ctx, "connected to Temporal, starting worker", slog.String("task_queue", taskQueue))
	healthServer.SetReady()

	if err := w.Run(interruptCh); err != nil {
		return fmt.Errorf("worker stopped: %w", err)
	}

	// Hold the /metrics endpoint open for one Prometheus scrape after drain so
	// end-of-lifecycle metrics are observed before exporter shutdown. Use
	// context.Background() because ctx is cancelled by SIGTERM, which would
	// otherwise abort the wait we are explicitly trying to perform.
	if err := metricsProvider.WaitForScrape(context.Background()); err != nil {
		slog.WarnContext(ctx, "scrape-on-shutdown gate did not observe a scrape before timeout", slog.Any("err", err))
	} else {
		slog.InfoContext(ctx, "final Prometheus scrape observed after drain")
	}

	return nil
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
//
// MEDIA_HARDWARE_DEVICE_PATH is overloaded: QSV/VAAPI take a filesystem path,
// but NVENC takes a CUDA ordinal string like "0" or "1" (see
// docs/hardware-acceleration.md). The backend is auto-selected at activity
// time from the encoders FFmpeg exposes, so we cannot tell at startup which
// interpretation will apply — a value that parses as a non-negative decimal
// integer is treated as the ordinal form and skipped rather than rejected as
// a missing file.
func validateHardwareDevicePath(path string) error {
	if path == "" {
		return nil
	}

	if _, err := strconv.ParseUint(path, 10, 32); err == nil {
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
