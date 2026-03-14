package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/workflows"
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

	client, err := hatchet.NewClient()
	if err != nil {
		return fmt.Errorf("create Hatchet client: %w", err)
	}

	worker, err := client.NewWorker(
		"mediaprocessor-worker",
		hatchet.WithWorkflows(workflows.NewPlaceholder(client)),
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
