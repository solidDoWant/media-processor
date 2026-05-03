package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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

	if err := run(ctx, worker.InterruptCh()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, interruptCh <-chan interface{}) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	logging.Setup(cfg.LogLevel)

	healthServer, err := health.New(ctx, cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("init health server: %w", err)
	}

	// Default to :9090 (paired with the worker's health port at :8080) so
	// running the watcher and worker side-by-side on the same host doesn't
	// collide on the metrics port. METRICS_ADDR overrides this.
	const defaultMetricsAddr = ":9090"

	metricsProvider, shutdown, err := metrics.NewFromEnv(defaultMetricsAddr)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer shutdown()

	radarrClient := radarr.New(cfg.Radarr)
	sonarrClient := sonarr.New(cfg.Sonarr)
	webhookClient := &webhook.Client{URL: cfg.WebhookURL}

	activities, err := media.NewActivities(cfg.Workflow, radarrClient, sonarrClient, webhookClient)
	if err != nil {
		return fmt.Errorf("init activities: %w", err)
	}

	temporalClient, shutdownTemporal, err := temporalclient.Dial(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		return err
	}
	defer shutdownTemporal()

	w := worker.New(temporalClient, cfg.TaskQueue, worker.Options{
		WorkerStopTimeout: cfg.WorkerStopTimeout,
	})

	activities.Register(w)

	slog.InfoContext(ctx, "connected to Temporal, starting worker", slog.String("task_queue", cfg.TaskQueue))
	healthServer.SetReady()

	if err := w.Run(interruptCh); err != nil {
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
