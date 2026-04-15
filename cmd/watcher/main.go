package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	v0Client "github.com/hatchet-dev/hatchet/pkg/client" //nolint:staticcheck // needed for WithLogLevel; no new-SDK equivalent
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/logging"
	"github.com/solidDoWant/media-processor/pkg/metrics"
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
	logging.Setup(os.Getenv("LOG_LEVEL"))

	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.Info("loaded watch mappings", "count", len(cfg.Watches), "config", configPath)

	if err := validateWatchDirs(cfg); err != nil {
		return fmt.Errorf("invalid watch configuration: %w", err)
	}

	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		return fmt.Errorf("HATCHET_CLIENT_TOKEN is not set")
	}

	metricsProvider, shutdown, err := metrics.NewFromEnv(ctx)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer shutdown()

	clientLogger := logging.NewZerologLogger("client")

	client, err := hatchet.NewClient(
		v0Client.WithLogger(&clientLogger), //nolint:staticcheck // no new-SDK equivalent for WithLogger
	)
	if err != nil {
		return fmt.Errorf("connect to Hatchet: %w", err)
	}

	slog.Info("connected to Hatchet")

	scanWorkflow, err := NewScanWorkflow(client, cfg, metricsProvider.MeterProvider())
	if err != nil {
		return fmt.Errorf("create scan workflow: %w", err)
	}

	watcherLogger := logging.NewZerologLogger("watcher")

	worker, err := client.NewWorker("mediaprocessor-watcher",
		hatchet.WithLogger(&watcherLogger),
		hatchet.WithWorkflows(scanWorkflow),
	)
	if err != nil {
		return fmt.Errorf("create watcher worker: %w", err)
	}

	slog.Info("starting directory scan worker", "schedule", cfg.CronSchedule)

	if err := worker.StartBlocking(ctx); err != nil {
		return fmt.Errorf("watcher stopped: %w", err)
	}

	return nil
}
