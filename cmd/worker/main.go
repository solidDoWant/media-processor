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

	v0Client "github.com/hatchet-dev/hatchet/pkg/client" //nolint:staticcheck // needed for WithLogger; no new-SDK equivalent
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/health"
	"github.com/solidDoWant/media-processor/pkg/logging"
	"github.com/solidDoWant/media-processor/pkg/medialib/radarr"
	"github.com/solidDoWant/media-processor/pkg/medialib/sonarr"
	"github.com/solidDoWant/media-processor/pkg/metrics"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows"
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

	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		return fmt.Errorf("HATCHET_CLIENT_TOKEN is not set")
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

	metricsProvider, shutdown, err := metrics.NewFromEnv(ctx)
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

	clientLogger := logging.NewZerologLogger("client")

	client, err := hatchet.NewClient(
		v0Client.WithLogger(&clientLogger), //nolint:staticcheck // no new-SDK equivalent for WithLogger
	)
	if err != nil {
		return fmt.Errorf("create Hatchet client: %w", err)
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

	mediaWorkflow := media.NewMediaWorkflow(client, media.MediaWorkflowConfig{
		WebhookURL:            webhookClient.URL,
		HardwareDevicePath:    os.Getenv("MEDIA_HARDWARE_DEVICE_PATH"),
		MeterProvider:         metricsProvider.MeterProvider(),
		HighCardinalityLabels: os.Getenv("METRICS_HIGH_CARDINALITY_LABELS") == "true",
		MinCropX:              minCropX,
		MinCropY:              minCropY,
		DetectCropTimeout:     detectCropTimeout,
		TranscodeTimeout:      transcodeTimeout,
		H265CRF:               h265CRF,
		ProgressLogInterval:   progressLogInterval,
	}, radarrClient, sonarrClient, webhookClient)

	workerLogger := logging.NewZerologLogger("worker")

	worker, err := client.NewWorker(
		"mediaprocessor-worker",
		hatchet.WithLogger(&workerLogger),
		hatchet.WithWorkflows(workflows.NewPlaceholder(client), mediaWorkflow),
	)
	if err != nil {
		return fmt.Errorf("create Hatchet worker: %w", err)
	}

	slog.InfoContext(ctx, "connected to Hatchet, starting worker")

	healthServer.SetReady()

	if err := worker.StartBlocking(ctx); err != nil {
		return fmt.Errorf("worker stopped: %w", err)
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
