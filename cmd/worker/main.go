package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/medialib/radarr"
	"github.com/solidDoWant/media-processor/pkg/webhook"
	"github.com/solidDoWant/media-processor/workflows"
	"github.com/solidDoWant/media-processor/workflows/movie"
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
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		return fmt.Errorf("HATCHET_CLIENT_TOKEN is not set")
	}

	movieOutputDir := os.Getenv("MOVIE_OUTPUT_DIR")
	if movieOutputDir == "" {
		return fmt.Errorf("MOVIE_OUTPUT_DIR is not set")
	}

	radarrURL := os.Getenv("RADARR_URL")
	if radarrURL == "" {
		return fmt.Errorf("RADARR_URL is not set")
	}

	radarrAPIKey := os.Getenv("RADARR_API_KEY")
	if radarrAPIKey == "" {
		return fmt.Errorf("RADARR_API_KEY is not set")
	}

	radarrClient := radarr.New(radarr.Config{
		URL:              radarrURL,
		APIKey:           radarrAPIKey,
		LocalPathPrefix:  os.Getenv("RADARR_LOCAL_PATH_PREFIX"),
		RemotePathPrefix: os.Getenv("RADARR_REMOTE_PATH_PREFIX"),
	})

	webhookClient := &webhook.Client{
		URL: os.Getenv("WEBHOOK_URL"),
	}

	client, err := hatchet.NewClient()
	if err != nil {
		return fmt.Errorf("create Hatchet client: %w", err)
	}

	movieWorkflow := movie.NewMovieWorkflow(client, movie.MovieWorkflowConfig{
		OutputDir:  movieOutputDir,
		WebhookURL: webhookClient.URL,
	}, radarrClient, webhookClient)

	worker, err := client.NewWorker(
		"mediaprocessor-worker",
		hatchet.WithWorkflows(workflows.NewPlaceholder(client), movieWorkflow),
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
