package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/media-processor/pkg/health"
	"github.com/solidDoWant/media-processor/pkg/loadprobe"
	"github.com/solidDoWant/media-processor/pkg/logging"
	"github.com/solidDoWant/media-processor/pkg/medialib/radarr"
	"github.com/solidDoWant/media-processor/pkg/medialib/sonarr"
	"github.com/solidDoWant/media-processor/pkg/metrics"
	"github.com/solidDoWant/media-processor/pkg/temporalclient"
	"github.com/solidDoWant/media-processor/pkg/transcodelimiter"
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

	// Derive a cancellable child context so the idle-exit poller can trigger
	// the same drain path as SIGTERM by cancelling it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	registerActivityEnabledMetric(metricsProvider.PrometheusRegisterer(), cfg.EnabledTokens)

	radarrClient := radarr.New(cfg.Radarr)
	sonarrClient := sonarr.New(cfg.Sonarr)
	webhookClient := &webhook.Client{URL: cfg.WebhookURL}

	// Auto-detection only runs on workers that actually transcode; on workers
	// that don't, the logged result would be misleading and the path is unused.
	transcodeEnabled := slices.Contains(cfg.EnabledTokens, media.TranscodeActivityToken)

	var hardwareDevicePath string
	if transcodeEnabled {
		hardwareDevicePath = resolveHardwareDevicePath(ctx, cfg.HardwareDevicePathOverride, defaultDRMRoot)
	}

	activities, err := media.NewActivities(
		cfg.Workflow,
		radarrClient,
		sonarrClient,
		webhookClient,
		media.WithHardwareDevicePath(hardwareDevicePath),
	)
	if err != nil {
		return fmt.Errorf("init activities: %w", err)
	}

	temporalClient, shutdownTemporal, err := temporalclient.Dial(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		return err
	}
	defer shutdownTemporal()

	var transcodeRuntime *transcodeLimiterRuntime
	if transcodeEnabled {
		transcodeRuntime = startTranscodeLimiter(ctx, hardwareDevicePath, cfg.TranscodeLimiter, metricsProvider.PrometheusRegisterer())
		defer transcodeRuntime.stop()
	}

	var workerInterceptors []interceptor.WorkerInterceptor

	if cfg.IdleExitAfter > 0 {
		tracker := newIdleTracker(nil)
		workerInterceptors = append(workerInterceptors, newIdleInterceptor(tracker))

		gauge := registerIdleGauge(metricsProvider.PrometheusRegisterer())

		ticker := time.NewTicker(idlePollInterval)
		defer ticker.Stop()

		poller := newIdlePoller(tracker, cfg.IdleExitAfter, gauge, ticker.C, cancel, slog.Default())

		go poller.run(ctx)

		slog.InfoContext(ctx, "idle-exit enabled", slog.Duration("idle_after", cfg.IdleExitAfter))
	}

	started, err := startWorkers(temporalClient, activities, cfg, transcodeRuntime, workerInterceptors)
	if err != nil {
		return err
	}

	// Closure (not direct call) so the deferred stop reads `started` at
	// invocation time, not at defer-statement time. Setting started = nil
	// after the explicit shutdown below makes this defer a safe no-op when
	// run() exits cleanly, while still draining workers if we return early
	// from a later step.
	defer func() {
		stopWorkers(ctx, started)
	}()

	queues := make([]string, len(started))
	for index, startedWorker := range started {
		queues[index] = startedWorker.label
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
// registered. The transcode-token worker additionally carries the GPU-aware
// SlotSupplier supplied by transcodeRuntime; other tokens use Temporal's
// default tuner. If any Worker fails to start, already-started Workers are
// stopped and the error is returned.
func startWorkers(c client.Client, activities *media.Activities, cfg workerConfig, transcodeRuntime *transcodeLimiterRuntime, interceptors []interceptor.WorkerInterceptor) ([]startedWorker, error) {
	started := make([]startedWorker, 0, len(cfg.EnabledTokens))

	baseOpts := worker.Options{
		WorkerStopTimeout: cfg.WorkerStopTimeout,
		Interceptors:      interceptors,
	}

	for _, token := range cfg.EnabledTokens {
		var (
			queue string
			label string
			w     worker.Worker
		)

		opts := baseOpts

		if token == media.WorkflowToken {
			queue = cfg.TaskQueuePrefix
			label = "workflow:" + queue
			w = worker.New(c, queue, opts)
			activities.RegisterWorkflow(w)
		} else {
			queue = media.ActivityTaskQueue(cfg.TaskQueuePrefix, token)
			label = "activity:" + queue

			if token == media.TranscodeActivityToken && transcodeRuntime != nil {
				opts.Tuner = transcodeRuntime.tuner
			}

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
	for index := len(started) - 1; index >= 0; index-- {
		slog.InfoContext(ctx, "stopping worker", slog.String("queue", started[index].label))
		started[index].w.Stop()
	}
}

// transcodeLimiterRuntime bundles the load-probe sampler, the slot-limiter,
// and the Temporal tuner that wraps it. Constructed only on transcode-enabled
// workers. The sampler owns the probe (probe.Close runs as part of
// sampler.Close), so this struct only needs to stop the limiter and sampler.
type transcodeLimiterRuntime struct {
	tuner   worker.WorkerTuner
	limiter *transcodelimiter.Limiter
	sampler *loadprobe.Sampler
}

// stop tears down the limiter then the sampler. Safe to call after a partial
// construction (any field may be nil) and safe to call multiple times.
func (r *transcodeLimiterRuntime) stop() {
	if r == nil {
		return
	}

	if r.limiter != nil {
		r.limiter.Close()
	}

	if r.sampler != nil {
		_ = r.sampler.Close()
	}
}

// startTranscodeLimiter constructs the load probe, sampler, and limiter for a
// transcode-enabled worker. A probe initialization failure is folded into a
// loadprobe.Failed sampler so the worker still boots — in static-cap-only
// mode — rather than refusing to start. The returned runtime always has a
// non-nil tuner suitable for attachment to the transcode worker's Options.
func startTranscodeLimiter(ctx context.Context, hardwareDevicePath string, cfg transcodeLimiterConfig, reg prometheus.Registerer) *transcodeLimiterRuntime {
	probe, probeErr := buildLoadProbe(hardwareDevicePath)

	var sampler *loadprobe.Sampler

	switch {
	case probeErr != nil:
		slog.WarnContext(ctx, "load probe init failed; transcode limiter will run in static-cap-only mode",
			slog.Any("error", probeErr),
		)

		sampler = loadprobe.Failed(probeErr, slog.Default())
	default:
		sampler = loadprobe.NewSampler(probe, loadprobe.SamplerConfig{
			Interval:        cfg.SampleInterval,
			SmoothingWindow: cfg.SmoothingWindow,
			Logger:          slog.Default(),
		})
		sampler.Start(ctx)
	}

	limiter, err := transcodelimiter.New(cfg.Limiter, sampler, reg, transcodelimiter.WithLogger(slog.Default()))
	if err != nil {
		// The only error path is "nil sampler", which we never produce. Log
		// the surprise, close the sampler (which would otherwise leak the
		// probe and the sampling goroutine), and fall through with a nil
		// limiter — the workers will run with Temporal's default tuner.
		slog.ErrorContext(ctx, "transcodelimiter.New failed unexpectedly", slog.Any("error", err))

		_ = sampler.Close()

		return &transcodeLimiterRuntime{}
	}

	tuner, err := buildTranscodeTuner(limiter)
	if err != nil {
		slog.ErrorContext(ctx, "build transcode tuner failed unexpectedly", slog.Any("error", err))
		limiter.Close()
		_ = sampler.Close()

		return &transcodeLimiterRuntime{}
	}

	return &transcodeLimiterRuntime{
		tuner:   tuner,
		limiter: limiter,
		sampler: sampler,
	}
}

// defaultSlotPoolSize is the per-pool slot count used by the SDK's
// NewFixedSizeTuner for workflow / local-activity / nexus / session pools.
// CompositeTuner requires every supplier to be non-nil, so we re-create the
// SDK's fixed-size suppliers for the slots we don't want to customize.
const defaultSlotPoolSize = 1000

// buildTranscodeTuner wraps the limiter as the activity slot supplier and
// supplies SDK-default fixed-size suppliers for the remaining four pools.
// CompositeTuner does not tolerate nil suppliers (downstream code panics on
// the first poll), so every field must be filled.
func buildTranscodeTuner(activitySupplier worker.SlotSupplier) (worker.WorkerTuner, error) {
	defaults := make([]worker.SlotSupplier, 0, 4)

	for index := 0; index < 4; index++ {
		supplier, err := worker.NewFixedSizeSlotSupplier(defaultSlotPoolSize)
		if err != nil {
			return nil, fmt.Errorf("build fixed-size slot supplier: %w", err)
		}

		defaults = append(defaults, supplier)
	}

	return worker.NewCompositeTuner(worker.CompositeTunerOptions{
		WorkflowSlotSupplier:        defaults[0],
		ActivitySlotSupplier:        activitySupplier,
		LocalActivitySlotSupplier:   defaults[1],
		NexusSlotSupplier:           defaults[2],
		SessionActivitySlotSupplier: defaults[3],
	})
}

// buildLoadProbe selects the probe implementation by hardware-device-path
// presence: a non-empty path implies an Intel render node and selects the
// i915 PMU probe; an empty path means software-only mode and selects the
// cgroup v2 CPU probe. Either constructor's error propagates so the caller
// can substitute a Failed sampler.
func buildLoadProbe(hardwareDevicePath string) (loadprobe.Probe, error) {
	if hardwareDevicePath != "" {
		return loadprobe.NewIntelProbe(hardwareDevicePath, loadprobe.IntelOptions{})
	}

	return loadprobe.NewCgroupProbe(loadprobe.CgroupOptions{})
}

// registerActivityEnabledMetric publishes one media_worker_activity_enabled
// series per known activity, set to 1 when the worker has the activity
// enabled and 0 when it doesn't. Emitted on every worker pod regardless of
// activity set so `sum by (activity) (media_worker_activity_enabled == 1)`
// is the count of pods running each activity.
func registerActivityEnabledMetric(reg prometheus.Registerer, enabledTokens []string) {
	if reg == nil {
		return
	}

	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "media_worker_activity_enabled",
		Help: "1 if this worker pod has the named activity enabled, 0 otherwise. One series per known activity.",
	}, []string{"activity"})

	reg.MustRegister(gauge)

	for _, activity := range media.KnownActivities {
		value := 0.0
		if slices.Contains(enabledTokens, activity) {
			value = 1
		}

		gauge.WithLabelValues(activity).Set(value)
	}
}
