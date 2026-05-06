package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/workflow"
)

const (
	// idleGaugeName is the Prometheus gauge name exposed when idle-exit is enabled.
	idleGaugeName = "media_worker_idle_exit_seconds_remaining"

	// idlePollInterval is how often the poll loop wakes to update the gauge
	// and check whether the idle window has elapsed.
	idlePollInterval = 5 * time.Second
)

// idleTracker counts in-flight activity/workflow tasks and records the
// wall-clock time of the most recent task lifecycle event (start or end).
// Tracking lastActivity on end as well as start means the idle window counts
// from when the worker becomes truly idle, not from when its last long-running
// task happened to start — so a task that outlives the idle window does not
// cause an immediate exit the moment it returns.
//
// Idle-exit is gated on the worker having processed at least one task: a
// freshly-started worker that has not yet seen any work stays alive
// indefinitely. This avoids spuriously draining a pod that has just been
// spawned (e.g. by KEDA) before its first task arrives, and matches the
// operator-facing meaning of "idle" — the worker is between jobs, not still
// warming up.
//
// The clock is injected so unit tests can advance time without sleeping.
type idleTracker struct {
	now func() time.Time

	mu           sync.Mutex
	inFlight     int64
	seen         bool
	lastActivity time.Time
}

// newIdleTracker returns an empty tracker. Idle-exit will not fire until the
// first markStart records a task. When `now` is nil, time.Now is used.
func newIdleTracker(now func() time.Time) *idleTracker {
	if now == nil {
		now = time.Now
	}

	return &idleTracker{now: now}
}

func (t *idleTracker) markStart() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inFlight++
	t.seen = true
	t.lastActivity = t.now()
}

func (t *idleTracker) markEnd() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inFlight--
	t.lastActivity = t.now()
}

// snapshot returns whether the worker has ever seen a task, the current
// in-flight count, and the elapsed time since the most recent task lifecycle
// event, taken atomically. When seen is false, elapsed is meaningless and
// callers must treat the worker as "still warming up" (do not idle-exit).
func (t *idleTracker) snapshot() (seen bool, inFlight int64, elapsed time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.seen {
		return false, t.inFlight, 0
	}

	return true, t.inFlight, t.now().Sub(t.lastActivity)
}

// idleInterceptor wraps activity and workflow execution to feed the tracker.
// It is attached to every started worker via worker.Options.Interceptors.
// Workers that never poll the workflow queue will only invoke InterceptActivity
// and vice-versa, so a single interceptor type covers both worker shapes.
type idleInterceptor struct {
	interceptor.WorkerInterceptorBase
	tracker *idleTracker
}

func newIdleInterceptor(tracker *idleTracker) interceptor.WorkerInterceptor {
	return &idleInterceptor{tracker: tracker}
}

func (i *idleInterceptor) InterceptActivity(_ context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &idleActivityInterceptor{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
		tracker:                        i.tracker,
	}
}

func (i *idleInterceptor) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	return &idleWorkflowInterceptor{
		WorkflowInboundInterceptorBase: interceptor.WorkflowInboundInterceptorBase{Next: next},
		tracker:                        i.tracker,
	}
}

type idleActivityInterceptor struct {
	interceptor.ActivityInboundInterceptorBase
	tracker *idleTracker
}

func (a *idleActivityInterceptor) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (interface{}, error) {
	a.tracker.markStart()
	defer a.tracker.markEnd()

	return a.Next.ExecuteActivity(ctx, in)
}

type idleWorkflowInterceptor struct {
	interceptor.WorkflowInboundInterceptorBase
	tracker *idleTracker
}

func (w *idleWorkflowInterceptor) ExecuteWorkflow(ctx workflow.Context, in *interceptor.ExecuteWorkflowInput) (interface{}, error) {
	w.tracker.markStart()
	defer w.tracker.markEnd()

	return w.Next.ExecuteWorkflow(ctx, in)
}

// registerIdleGauge registers the idle-exit countdown gauge on the supplied
// registerer. When reg is nil (metrics disabled), no gauge is created and a
// nil gauge is returned — the poll loop tolerates a nil gauge.
func registerIdleGauge(reg prometheus.Registerer) prometheus.Gauge {
	if reg == nil {
		return nil
	}

	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: idleGaugeName,
		Help: "Seconds remaining before the worker initiates an idle-exit drain. Held at the configured WORKER_IDLE_EXIT_AFTER value while activity or workflow-task work is in flight.",
	})

	reg.MustRegister(g)

	return g
}

// idlePoller periodically inspects an idleTracker and triggers a drain by
// calling cancel when the idle window has elapsed with zero in-flight tasks.
// Each tick also updates the countdown gauge.
type idlePoller struct {
	tracker   *idleTracker
	idleAfter time.Duration
	gauge     prometheus.Gauge
	tick      <-chan time.Time
	cancel    context.CancelFunc
	logger    *slog.Logger
}

func newIdlePoller(tracker *idleTracker, idleAfter time.Duration, gauge prometheus.Gauge, tick <-chan time.Time, cancel context.CancelFunc, logger *slog.Logger) *idlePoller {
	if logger == nil {
		logger = slog.Default()
	}

	return &idlePoller{
		tracker:   tracker,
		idleAfter: idleAfter,
		gauge:     gauge,
		tick:      tick,
		cancel:    cancel,
		logger:    logger,
	}
}

// run drives the poll loop until ctx is cancelled or evaluate returns true.
// It is safe to invoke from a goroutine; the only side effects are the gauge
// update and the cancel call.
func (p *idlePoller) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.tick:
			if p.evaluate() {
				return
			}
		}
	}
}

// evaluate updates the gauge to the current remaining-seconds value and
// returns true after triggering cancel on the first tick where the idle
// window has elapsed with zero in-flight tasks AND the worker has processed
// at least one task. A worker that has never seen a task is held at the
// configured idleAfter (timer paused) until its first task arrives. A nil-but-
// already-cancelled context is fine: callers can call cancel() multiple times
// safely.
func (p *idlePoller) evaluate() bool {
	seen, inFlight, elapsed := p.tracker.snapshot()

	remaining := p.idleAfter
	if seen && inFlight == 0 {
		remaining = p.idleAfter - elapsed
		if remaining < 0 {
			remaining = 0
		}
	}

	if p.gauge != nil {
		p.gauge.Set(remaining.Seconds())
	}

	if seen && inFlight == 0 && elapsed >= p.idleAfter {
		p.logger.Info("idle-exit window elapsed; initiating drain",
			slog.Duration("idle_after", p.idleAfter),
			slog.Duration("elapsed", elapsed),
		)

		p.cancel()

		return true
	}

	return false
}
