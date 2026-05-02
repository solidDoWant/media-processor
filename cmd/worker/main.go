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

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logging.Setup(os.Getenv("LOG_LEVEL"))

	taskQueue := os.Getenv("TEMPORAL_TASK_QUEUE")
	if taskQueue == "" {
		return fmt.Errorf("TEMPORAL_TASK_QUEUE is not set")
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

	metricsProvider, shutdown, err := metrics.NewFromEnv()
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer shutdown()

	radarrURL := os.Getenv("RADARR_URL")
	if radarrURL == "" {
		return fmt.Errorf("RADARR_URL is not set")
	}

	radarrAPIKey := os.Getenv("RADARR_API_KEY")
	if radarrAPIKey == "" {
		return fmt.Errorf("RADARR_API_KEY is not set")
	}

	sonarrURL := os.Getenv("SONARR_URL")
	if sonarrURL == "" {
		return fmt.Errorf("SONARR_URL is not set")
	}

	sonarrAPIKey := os.Getenv("SONARR_API_KEY")
	if sonarrAPIKey == "" {
		return fmt.Errorf("SONARR_API_KEY is not set")
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

	activities, err := media.NewActivities(media.MediaWorkflowConfig{
		HardwareDevicePath:    os.Getenv("MEDIA_HARDWARE_DEVICE_PATH"),
		MetricsRegisterer:     metricsProvider.PrometheusRegisterer(),
		HighCardinalityLabels: os.Getenv("METRICS_HIGH_CARDINALITY_LABELS") == "true",
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

	// Forward context cancellation (SIGINT/SIGTERM via signal.NotifyContext)
	// onto the worker's interrupt channel. worker.Run starts the worker, blocks
	// until the channel receives, and then performs a graceful Stop that drains
	// in-flight activities.
	interrupt := make(chan interface{}, 1)

	go func() {
		<-ctx.Done()

		interrupt <- struct{}{}
	}()

	slog.InfoContext(ctx, "connected to Temporal, starting worker", slog.String("task_queue", taskQueue))
	healthServer.SetReady()

	if err := w.Run(interrupt); err != nil {
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
