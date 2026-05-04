package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.temporal.io/sdk/client"
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

	started, err := startWorkers(temporalClient, activities, cfg)
	if err != nil {
		return err
	}

	defer stopWorkers(ctx, started)

	queues := make([]string, len(started))
	for i, sw := range started {
		queues[i] = sw.label
	}

	slog.InfoContext(ctx, "connected to Temporal, starting workers", slog.Any("queues", queues))
	healthServer.SetReady()

	select {
	case <-ctx.Done():
	case <-interruptCh:
	}

	slog.InfoContext(ctx, "stopping workers")
	stopWorkers(ctx, started)
	started = nil // suppress the deferred stop now that drain has completed

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

// startedWorker pairs a Temporal Worker with the queue label used in logs and
// errors. Activity workers carry their queue name; the workflow worker uses
// "workflow:{prefix}" so the two cases are distinguishable.
type startedWorker struct {
	label string
	w     worker.Worker
}

// startWorkers builds and starts one Temporal Worker per token in
// cfg.EnabledTokens. The workflow token (if present) yields a Worker on the
// prefix-only queue with only the workflow function registered; each activity
// token yields a Worker on its activity-specific queue with only that activity
// registered. If any Worker fails to start, already-started Workers are stopped
// and the error is returned.
func startWorkers(c client.Client, activities *media.Activities, cfg workerConfig) ([]startedWorker, error) {
	started := make([]startedWorker, 0, len(cfg.EnabledTokens))

	opts := worker.Options{WorkerStopTimeout: cfg.WorkerStopTimeout}

	for _, token := range cfg.EnabledTokens {
		var (
			queue string
			label string
			w     worker.Worker
		)

		if token == media.WorkflowToken {
			queue = cfg.TaskQueuePrefix
			label = "workflow:" + queue
			w = worker.New(c, queue, opts)
			activities.RegisterWorkflow(w)
		} else {
			queue = media.ActivityTaskQueue(cfg.TaskQueuePrefix, token)
			label = "activity:" + queue
			w = worker.New(c, queue, opts)

			if err := activities.RegisterActivity(w, token); err != nil {
				stopWorkers(context.Background(), started)
				return nil, fmt.Errorf("register %s: %w", token, err)
			}
		}

		if err := w.Start(); err != nil {
			stopWorkers(context.Background(), started)
			return nil, fmt.Errorf("start %s: %w", label, err)
		}

		started = append(started, startedWorker{label: label, w: w})
	}

	return started, nil
}

// stopWorkers stops every started Worker in reverse order (LIFO). Each Stop
// call blocks for at most WorkerStopTimeout while the SDK drains in-flight
// activities, so calling sequentially keeps shutdown ordering predictable.
func stopWorkers(ctx context.Context, started []startedWorker) {
	for i := len(started) - 1; i >= 0; i-- {
		slog.InfoContext(ctx, "stopping worker", slog.String("queue", started[i].label))
		started[i].w.Stop()
	}
}
