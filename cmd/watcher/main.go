package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/media-processor/pkg/health"
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

	taskQueue := os.Getenv("TEMPORAL_TASK_QUEUE")
	if taskQueue == "" {
		return fmt.Errorf("TEMPORAL_TASK_QUEUE is not set")
	}

	const defaultHealthAddr = ":8081"

	healthAddr := os.Getenv("HEALTH_ADDR")
	if healthAddr == "" {
		healthAddr = defaultHealthAddr
	}

	healthServer, err := health.New(ctx, healthAddr)
	if err != nil {
		return fmt.Errorf("init health server: %w", err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.Info("loaded watch mappings", "count", len(cfg.Watches), "config", configPath)

	if err := validateWatchDirs(cfg); err != nil {
		return fmt.Errorf("invalid watch configuration: %w", err)
	}

	metricsProvider, shutdown, err := metrics.NewFromEnv(ctx)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer shutdown()

	instruments, err := newScanInstruments(metricsProvider.MeterProvider())
	if err != nil {
		return fmt.Errorf("register scan metrics: %w", err)
	}

	temporalClient, err := client.Dial(client.Options{
		HostPort:  os.Getenv("TEMPORAL_ADDRESS"),
		Namespace: os.Getenv("TEMPORAL_NAMESPACE"),
	})
	if err != nil {
		return fmt.Errorf("dial Temporal: %w", err)
	}
	defer temporalClient.Close()

	// CheckHealth verifies the gRPC connection to the frontend before the
	// scan loop begins; without it, Dial returns successfully even when the
	// server is unreachable and the watcher silently spins.
	healthCheckCtx, healthCheckCancel := context.WithTimeout(ctx, 10*time.Second)

	_, healthErr := temporalClient.CheckHealth(healthCheckCtx, &client.CheckHealthRequest{})

	healthCheckCancel()

	if healthErr != nil {
		return fmt.Errorf("temporal health check failed: %w", healthErr)
	}

	slog.InfoContext(ctx, "connected to Temporal, starting scan loop",
		slog.String("task_queue", taskQueue),
		slog.Duration("interval", cfg.ScanInterval.Duration()),
	)

	healthServer.SetReady()

	dispatch := newTemporalDispatch(temporalClient, taskQueue)
	runScanLoop(ctx, cfg, instruments, dispatch)

	return nil
}
