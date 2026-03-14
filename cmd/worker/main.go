package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/workflows"
)

func main() {
	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		log.Fatal("HATCHET_CLIENT_TOKEN is not set")
	}

	client, err := hatchet.NewClient()
	if err != nil {
		log.Fatalf("failed to create Hatchet client: %v", err)
	}

	worker, err := client.NewWorker(
		"mediaprocessor-worker",
		hatchet.WithWorkflows(workflows.NewPlaceholder(client)),
	)
	if err != nil {
		log.Fatalf("failed to create Hatchet worker: %v", err)
	}

	log.Println("connected to Hatchet, starting worker")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := worker.StartBlocking(ctx); err != nil {
		log.Fatalf("worker stopped with error: %v", err)
	}
}
