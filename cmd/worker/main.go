package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

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
	metricsProvider, err := metrics.New(ctx)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := metricsProvider.Shutdown(ctx); err != nil {
			log.Printf("metrics shutdown error: %v", err)
		}
	}()

	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		return fmt.Errorf("HATCHET_CLIENT_TOKEN is not set")
	}

	mediaOutputDir := os.Getenv("MEDIA_OUTPUT_DIR")
	if mediaOutputDir == "" {
		return fmt.Errorf("MEDIA_OUTPUT_DIR is not set")
	}

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
		URL:              radarrURL,
		APIKey:           radarrAPIKey,
		LocalPathPrefix:  os.Getenv("RADARR_LOCAL_PATH_PREFIX"),
		RemotePathPrefix: os.Getenv("RADARR_REMOTE_PATH_PREFIX"),
	})

	sonarrClient := sonarr.New(sonarr.Config{
		URL:              sonarrURL,
		APIKey:           sonarrAPIKey,
		LocalPathPrefix:  os.Getenv("SONARR_LOCAL_PATH_PREFIX"),
		RemotePathPrefix: os.Getenv("SONARR_REMOTE_PATH_PREFIX"),
	})

	webhookClient := &webhook.Client{
		URL: os.Getenv("WEBHOOK_URL"),
	}

	client, err := hatchet.NewClient()
	if err != nil {
		return fmt.Errorf("create Hatchet client: %w", err)
	}

	mediaWorkflow := media.NewMediaWorkflow(client, media.MediaWorkflowConfig{
		OutputDir:          mediaOutputDir,
		WebhookURL:         webhookClient.URL,
		HardwareDevicePath: os.Getenv("HARDWARE_DEVICE_PATH"),
	}, radarrClient, sonarrClient, webhookClient)

	worker, err := client.NewWorker(
		"mediaprocessor-worker",
		hatchet.WithWorkflows(workflows.NewPlaceholder(client), mediaWorkflow),
	)
	if err != nil {
		return fmt.Errorf("create Hatchet worker: %w", err)
	}

	log.Println("connected to Hatchet, starting worker")

	if err := worker.StartBlocking(ctx); err != nil {
		return fmt.Errorf("worker stopped: %w", err)
	}

	return nil
}
