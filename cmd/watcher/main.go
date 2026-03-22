package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to watcher config file")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log.Printf("loaded %d watch mapping(s) from %s", len(cfg.Watches), configPath)

	if err := validateWatchDirs(cfg); err != nil {
		return fmt.Errorf("invalid watch configuration: %w", err)
	}

	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		return fmt.Errorf("HATCHET_CLIENT_TOKEN is not set")
	}

	client, err := hatchet.NewClient()
	if err != nil {
		return fmt.Errorf("connect to Hatchet: %w", err)
	}

	log.Println("connected to Hatchet")

	scanWorkflow := NewScanWorkflow(client, cfg)

	worker, err := client.NewWorker("mediaprocessor-watcher",
		hatchet.WithWorkflows(scanWorkflow),
	)
	if err != nil {
		return fmt.Errorf("create watcher worker: %w", err)
	}

	log.Printf("starting directory scan worker (schedule: %s)", cfg.CronSchedule)

	if err := worker.StartBlocking(ctx); err != nil {
		return fmt.Errorf("watcher stopped: %w", err)
	}

	return nil
}
