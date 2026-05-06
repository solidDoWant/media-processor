package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/workflow"
)

// fakeClock is a hand-driven clock for unit tests. The zero value starts at
// time.Unix(0, 0); callers advance it explicitly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// TestIdleTrackerLastActivityUpdatesOnEachLifecycleEvent verifies that
// markStart and markEnd both record the wall-clock time, so the elapsed
// window is measured from the most recent task event (start or end).
func TestIdleTrackerLastActivityUpdatesOnEachLifecycleEvent(t *testing.T) {
	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	clock.Advance(2 * time.Minute)
	tracker.markStart()
	tracker.markEnd()

	// Time advances; without a fresh event, elapsed grows from the last end.
	clock.Advance(30 * time.Second)

	seen, inFlight, elapsed := tracker.snapshot()
	assert.True(t, seen)
	assert.Equal(t, int64(0), inFlight)
	assert.Equal(t, 30*time.Second, elapsed)

	// A fresh start/end pair resets the window.
	clock.Advance(10 * time.Second)
	tracker.markStart()
	tracker.markEnd()

	seen, inFlight, elapsed = tracker.snapshot()
	assert.True(t, seen)
	assert.Equal(t, int64(0), inFlight)
	assert.Equal(t, time.Duration(0), elapsed)
}

// TestIdleTrackerInitialSnapshotIsUnseen verifies that a fresh tracker
// reports seen=false until the first markStart, which is what gates the
// poller's idle-exit decision while the worker is warming up.
func TestIdleTrackerInitialSnapshotIsUnseen(t *testing.T) {
	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	clock.Advance(time.Hour)

	seen, inFlight, _ := tracker.snapshot()
	assert.False(t, seen, "fresh tracker must report seen=false")
	assert.Equal(t, int64(0), inFlight)

	tracker.markStart()
	tracker.markEnd()

	seen, _, _ = tracker.snapshot()
	assert.True(t, seen, "after the first start, snapshot must report seen=true permanently")
}

// TestIdleTrackerInFlightCountReflectsConcurrent verifies that in-flight
// reflects the number of overlapping start/end pairs and only returns to zero
// once every started task has ended.
func TestIdleTrackerInFlightCountReflectsConcurrent(t *testing.T) {
	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	tracker.markStart()
	tracker.markStart()

	_, inFlight, _ := tracker.snapshot()
	assert.Equal(t, int64(2), inFlight)

	tracker.markEnd()

	_, inFlight, _ = tracker.snapshot()
	assert.Equal(t, int64(1), inFlight)

	tracker.markEnd()

	_, inFlight, _ = tracker.snapshot()
	assert.Equal(t, int64(0), inFlight)
}

// TestIdlePollerCancelsAfterIdleWindow verifies that once elapsed >= idleAfter
// and inFlight == 0, the first poll tick triggers cancel and run() returns.
// Earlier ticks (window not yet elapsed) only update the gauge and return false.
func TestIdlePollerCancelsAfterIdleWindow(t *testing.T) {
	const idleAfter = 5 * time.Minute

	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	cancelCalls := 0
	cancel := func() { cancelCalls++ }

	reg := prometheus.NewRegistry()
	gauge := registerIdleGauge(reg)

	poller := newIdlePoller(tracker, idleAfter, gauge, nil, cancel, slog.Default())

	// Worker has processed a task; idle countdown begins from this end.
	tracker.markStart()
	tracker.markEnd()

	// Tick before window elapses: no cancel, gauge counts down.
	clock.Advance(2 * time.Minute)
	assert.False(t, poller.evaluate())
	assert.Equal(t, 0, cancelCalls)
	assert.InDelta(t, 3*time.Minute.Seconds(), gaugeValue(t, reg), 0.001)

	// Tick after window elapses: cancel fires, gauge sits at zero.
	clock.Advance(4 * time.Minute)
	assert.True(t, poller.evaluate())
	assert.Equal(t, 1, cancelCalls)
	assert.InDelta(t, 0, gaugeValue(t, reg), 0.001)
}

// TestIdlePollerHoldsWhileWarmingUp verifies that a worker that has not yet
// seen any task does not idle-exit, regardless of how much wall-clock time
// has elapsed since the worker started. The countdown only begins once the
// first task has been observed (matches the operator-facing meaning of
// "idle" — between jobs, not still warming up).
func TestIdlePollerHoldsWhileWarmingUp(t *testing.T) {
	const idleAfter = time.Minute

	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	cancelCalls := 0
	cancel := func() { cancelCalls++ }

	reg := prometheus.NewRegistry()
	gauge := registerIdleGauge(reg)

	poller := newIdlePoller(tracker, idleAfter, gauge, nil, cancel, slog.Default())

	// Far longer than idleAfter has passed, but the worker has never seen a
	// task. The poller must not cancel; the gauge stays at idleAfter.
	clock.Advance(time.Hour)
	assert.False(t, poller.evaluate())
	assert.Equal(t, 0, cancelCalls)
	assert.InDelta(t, idleAfter.Seconds(), gaugeValue(t, reg), 0.001)

	// First task arrives. After it ends and the window elapses, cancel fires.
	tracker.markStart()
	tracker.markEnd()
	clock.Advance(2 * time.Minute)
	assert.True(t, poller.evaluate())
	assert.Equal(t, 1, cancelCalls)
}

// TestIdlePollerRearmsOnFreshStart verifies that an interleaved markStart
// resets lastActivity so the next idle window must start over from the new
// activity timestamp.
func TestIdlePollerRearmsOnFreshStart(t *testing.T) {
	const idleAfter = 5 * time.Minute

	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	cancelCalls := 0
	cancel := func() { cancelCalls++ }

	poller := newIdlePoller(tracker, idleAfter, nil, nil, cancel, slog.Default())

	// Almost-elapsed window, then a fresh start arrives.
	clock.Advance(4 * time.Minute)
	tracker.markStart()
	tracker.markEnd()

	// Original window would have elapsed by now, but the fresh start reset
	// lastStart, so the poller must not cancel yet.
	clock.Advance(3 * time.Minute)
	assert.False(t, poller.evaluate())
	assert.Equal(t, 0, cancelCalls)

	// A full new window elapses since the fresh start: cancel fires.
	clock.Advance(3 * time.Minute)
	assert.True(t, poller.evaluate())
	assert.Equal(t, 1, cancelCalls)
}

// TestIdlePollerHoldsWhileInFlight verifies the in-flight gate: even once
// elapsed exceeds idleAfter, an in-flight task prevents cancel; once the
// task ends and a fresh window elapses, cancel fires.
func TestIdlePollerHoldsWhileInFlight(t *testing.T) {
	const idleAfter = 5 * time.Minute

	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	cancelCalls := 0
	cancel := func() { cancelCalls++ }

	reg := prometheus.NewRegistry()
	gauge := registerIdleGauge(reg)

	poller := newIdlePoller(tracker, idleAfter, gauge, nil, cancel, slog.Default())

	// A long-running task is in flight when the would-be idle window elapses.
	tracker.markStart()
	clock.Advance(10 * time.Minute)

	assert.False(t, poller.evaluate())
	assert.Equal(t, 0, cancelCalls)

	// While in flight, the gauge is held at idleAfter — the timer is paused.
	assert.InDelta(t, idleAfter.Seconds(), gaugeValue(t, reg), 0.001)

	// Task completes; a fresh idle window must elapse from this point.
	tracker.markEnd()

	clock.Advance(2 * time.Minute)
	assert.False(t, poller.evaluate())
	assert.Equal(t, 0, cancelCalls)

	clock.Advance(4 * time.Minute)
	assert.True(t, poller.evaluate())
	assert.Equal(t, 1, cancelCalls)
}

// TestIdleInterceptorActivityTracksLifecycle verifies the activity wrapper
// updates the tracker on entry and exit, propagates the inner result and
// error, and records lastStart at the moment the activity began.
func TestIdleInterceptorActivityTracksLifecycle(t *testing.T) {
	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	wantErr := errors.New("inner failed")

	var seenInFlightInside int64

	innerFn := func(_ context.Context, _ *interceptor.ExecuteActivityInput) (interface{}, error) {
		_, seenInFlightInside, _ = tracker.snapshot()

		return "result", wantErr
	}

	wrapper := &idleActivityInterceptor{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{
			Next: &fakeActivityNext{exec: innerFn},
		},
		tracker: tracker,
	}

	clock.Advance(time.Minute)

	out, err := wrapper.ExecuteActivity(t.Context(), nil)
	assert.Equal(t, "result", out)
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, int64(1), seenInFlightInside, "tracker must show in-flight while inner is executing")

	seen, inFlight, elapsed := tracker.snapshot()
	assert.True(t, seen, "tracker must have observed the activity start")
	assert.Equal(t, int64(0), inFlight, "tracker must clear in-flight after inner returns")
	assert.Equal(t, time.Duration(0), elapsed, "lastActivity must reflect the activity's end time")
}

// TestIdleInterceptorWorkflowTracksLifecycle verifies the workflow wrapper
// updates the tracker on entry and exit, so workflow-task starts rearm the
// idle timer just as activity starts do.
func TestIdleInterceptorWorkflowTracksLifecycle(t *testing.T) {
	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	var seenInFlightInside int64

	innerFn := func(_ workflow.Context, _ *interceptor.ExecuteWorkflowInput) (interface{}, error) {
		_, seenInFlightInside, _ = tracker.snapshot()

		return nil, nil
	}

	wrapper := &idleWorkflowInterceptor{
		WorkflowInboundInterceptorBase: interceptor.WorkflowInboundInterceptorBase{
			Next: &fakeWorkflowNext{exec: innerFn},
		},
		tracker: tracker,
	}

	clock.Advance(2 * time.Minute)

	_, err := wrapper.ExecuteWorkflow(nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), seenInFlightInside)

	seen, inFlight, elapsed := tracker.snapshot()
	assert.True(t, seen)
	assert.Equal(t, int64(0), inFlight)
	assert.Equal(t, time.Duration(0), elapsed)
}

// TestIdleGaugeAbsentWhenRegistererNil verifies that when the metrics
// provider is in disabled mode (nil registerer), no gauge is constructed
// and the poller tolerates the nil gauge.
func TestIdleGaugeAbsentWhenRegistererNil(t *testing.T) {
	gauge := registerIdleGauge(nil)
	assert.Nil(t, gauge)

	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	cancelCalls := 0
	poller := newIdlePoller(tracker, time.Minute, nil, nil, func() { cancelCalls++ }, slog.Default())

	// Worker has done a task so the warmup gate is satisfied.
	tracker.markStart()
	tracker.markEnd()

	// Should not panic even though gauge is nil.
	clock.Advance(2 * time.Minute)
	assert.True(t, poller.evaluate())
	assert.Equal(t, 1, cancelCalls)
}

// TestIdlePollerRunStopsOnContextCancel verifies the goroutine exits cleanly
// when its own context is cancelled (e.g. the worker is shutting down for
// reasons other than idle-exit, like a real SIGTERM).
func TestIdlePollerRunStopsOnContextCancel(t *testing.T) {
	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	tickCh := make(chan time.Time)
	cancelCalls := 0

	poller := newIdlePoller(tracker, time.Minute, nil, tickCh, func() { cancelCalls++ }, slog.Default())

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		poller.run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "poller did not return after context cancellation")
	}

	assert.Equal(t, 0, cancelCalls, "context cancellation must not trigger an idle-exit drain")
}

// TestIdlePollerRunCancelsViaTick verifies the run loop wires the tick
// channel through evaluate and exits after triggering cancel.
func TestIdlePollerRunCancelsViaTick(t *testing.T) {
	const idleAfter = time.Minute

	clock := newFakeClock()
	tracker := newIdleTracker(clock.Now)

	tickCh := make(chan time.Time, 1)
	cancelCalls := 0
	cancel := func() { cancelCalls++ }

	poller := newIdlePoller(tracker, idleAfter, nil, tickCh, cancel, slog.Default())

	tracker.markStart()
	tracker.markEnd()

	done := make(chan struct{})

	go func() {
		poller.run(t.Context())
		close(done)
	}()

	clock.Advance(2 * time.Minute)

	tickCh <- clock.Now()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "poller did not exit after cancel-triggering tick")
	}

	assert.Equal(t, 1, cancelCalls)
}

// gaugeValue gathers a single-series gauge from the registry by name.
func gaugeValue(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != idleGaugeName {
			continue
		}

		metricList := family.GetMetric()
		require.Len(t, metricList, 1, "expected a single series for %s", idleGaugeName)

		return metricList[0].GetGauge().GetValue()
	}

	require.Failf(t, "missing idle-exit gauge", "gauge %q not registered", idleGaugeName)

	return 0
}

// TestIdleGaugeRegistered verifies that when registerIdleGauge is called
// with a real registry, the gauge is registered with the documented name
// and a non-empty help string. (The "absent when disabled" half is covered
// by main.go's gating and TestIdleGaugeAbsentWhenRegistererNil above.)
func TestIdleGaugeRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()

	gauge := registerIdleGauge(reg)
	require.NotNil(t, gauge)

	families, err := reg.Gather()
	require.NoError(t, err)

	var seen *dto.MetricFamily

	for _, family := range families {
		if family.GetName() == idleGaugeName {
			seen = family

			break
		}
	}

	require.NotNil(t, seen, "gauge %q not registered", idleGaugeName)
	assert.NotEmpty(t, seen.GetHelp(), "gauge must have a help string for operators")
}

// fakeActivityNext implements interceptor.ActivityInboundInterceptor for
// tests. Tests provide an exec callback so they can observe state at the
// moment the inner activity is running.
type fakeActivityNext struct {
	interceptor.ActivityInboundInterceptorBase
	exec func(ctx context.Context, in *interceptor.ExecuteActivityInput) (interface{}, error)
}

func (f *fakeActivityNext) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (interface{}, error) {
	return f.exec(ctx, in)
}

// fakeWorkflowNext is the workflow-side analogue of fakeActivityNext.
type fakeWorkflowNext struct {
	interceptor.WorkflowInboundInterceptorBase
	exec func(ctx workflow.Context, in *interceptor.ExecuteWorkflowInput) (interface{}, error)
}

func (f *fakeWorkflowNext) ExecuteWorkflow(ctx workflow.Context, in *interceptor.ExecuteWorkflowInput) (interface{}, error) {
	return f.exec(ctx, in)
}
