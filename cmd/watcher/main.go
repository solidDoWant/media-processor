package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/solidDoWant/media-processor/internal/envvar"
	"github.com/solidDoWant/media-processor/pkg/health"
	"github.com/solidDoWant/media-processor/pkg/logging"
	"github.com/solidDoWant/media-processor/pkg/metrics"
	"github.com/solidDoWant/media-processor/pkg/temporalclient"
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

	taskQueue, err := envvar.RequireEnv("TEMPORAL_TASK_QUEUE")
	if err != nil {
		return err
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

	// Default to :9091 (paired with the watcher's health port at :8081) so
	// running the watcher and worker side-by-side on the same host doesn't
	// collide on the metrics port. METRICS_ADDR overrides this.
	const defaultMetricsAddr = ":9091"

	metricsProvider, shutdown, err := metrics.NewFromEnv(defaultMetricsAddr)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer shutdown()

	instruments, err := newScanInstruments(metricsProvider.PrometheusRegisterer())
	if err != nil {
		return fmt.Errorf("register scan metrics: %w", err)
	}

	temporalClient, shutdownTemporal, err := temporalclient.Dial(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		return err
	}
	defer shutdownTemporal()

	searchAttrsEnabled := true
	if v := os.Getenv("WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED"); v != "" {
		searchAttrsEnabled, err = strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED %q: %w", v, err)
		}
	}

	slog.InfoContext(ctx, "connected to Temporal, starting scan loop",
		slog.String("task_queue", taskQueue),
		slog.Duration("interval", cfg.ScanInterval.Duration()),
		slog.Bool("search_attributes_enabled", searchAttrsEnabled),
	)

	healthServer.SetReady()

	dispatch := newTemporalDispatch(temporalClient, taskQueue, searchAttrsEnabled)
	runScanLoop(ctx, cfg, instruments, dispatch)

	// Hold the /metrics endpoint open for one Prometheus scrape after the scan
	// loop exits so end-of-lifecycle metrics are observed before exporter
	// shutdown. Use context.Background() because ctx is cancelled by SIGTERM,
	// which would otherwise abort the wait we are explicitly trying to perform.
	if err := metricsProvider.WaitForScrape(context.Background()); err != nil {
		slog.WarnContext(ctx, "scrape-on-shutdown gate did not observe a scrape before timeout", slog.Any("err", err))
	} else {
		slog.InfoContext(ctx, "final Prometheus scrape observed after scan loop exit")
	}

	return nil
}
