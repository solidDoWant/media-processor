package transcodelimiter_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/media-processor/pkg/transcodelimiter"
)

// fakeSampler is a deterministic Sampler for unit tests. Value is read with
// atomic loads so the limiter goroutine can observe value changes without a
// data race; FailedC is closed via fail() to drive the fallback transition.
type fakeSampler struct {
	value atomic.Uint64 // math.Float64bits-encoded

	mu     sync.Mutex
	failed chan struct{}
}

func newFakeSampler(initial float64) *fakeSampler {
	s := &fakeSampler{failed: make(chan struct{})}
	s.set(initial)

	return s
}

func (s *fakeSampler) Value() float64 {
	return math.Float64frombits(s.value.Load())
}

func (s *fakeSampler) FailedC() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failed
}

func (s *fakeSampler) set(v float64) {
	s.value.Store(math.Float64bits(v))
}

func (s *fakeSampler) fail() {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.failed:
		return
	default:
		close(s.failed)
	}
}

// failedSampler returns a Sampler that is already in fallback mode, so the
// limiter constructed around it starts in static-cap-only mode.
func failedSampler() *fakeSampler {
	s := newFakeSampler(0)
	s.fail()

	return s
}

func TestLimiterAdmitsBelowThreshold(t *testing.T) {
	sampler := newFakeSampler(0.2)
	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:             5,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: 0,
	}, sampler, nil, transcodelimiter.WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	permit, err := lim.ReserveSlot(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, permit)
}

func TestLimiterBlocksAtOrAboveThreshold(t *testing.T) {
	sampler := newFakeSampler(0.9)
	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:             5,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: 0,
	}, sampler, nil, transcodelimiter.WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	type result struct {
		permit *worker.SlotPermit
		err    error
	}

	resCh := make(chan result, 1)

	go func() {
		permit, reserveErr := lim.ReserveSlot(t.Context(), nil)
		resCh <- result{permit: permit, err: reserveErr}
	}()

	select {
	case r := <-resCh:
		t.Fatalf("ReserveSlot returned while above threshold: permit=%v err=%v", r.permit, r.err)
	case <-time.After(50 * time.Millisecond):
	}

	sampler.set(0.5)

	select {
	case r := <-resCh:
		require.NoError(t, r.err)
		require.NotNil(t, r.permit)
	case <-time.After(time.Second):
		t.Fatal("ReserveSlot did not unblock after value dropped below threshold")
	}
}

func TestLimiterBlocksWhenInFlightAtCap(t *testing.T) {
	sampler := newFakeSampler(0)
	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:             1,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: 0,
	}, sampler, nil, transcodelimiter.WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	_, err = lim.ReserveSlot(t.Context(), nil)
	require.NoError(t, err)
	lim.MarkSlotUsed(nil)

	type result struct {
		permit *worker.SlotPermit
		err    error
	}

	resCh := make(chan result, 1)

	go func() {
		permit, reserveErr := lim.ReserveSlot(t.Context(), nil)
		resCh <- result{permit: permit, err: reserveErr}
	}()

	select {
	case r := <-resCh:
		t.Fatalf("ReserveSlot returned while at cap: permit=%v err=%v", r.permit, r.err)
	case <-time.After(50 * time.Millisecond):
	}

	lim.ReleaseSlot(nil)

	select {
	case r := <-resCh:
		require.NoError(t, r.err)
		require.NotNil(t, r.permit)
	case <-time.After(time.Second):
		t.Fatal("ReserveSlot did not unblock after release")
	}
}

func TestLimiterCooldownEnforced(t *testing.T) {
	sampler := newFakeSampler(0.1)

	var (
		clockMu sync.Mutex
		now     = time.Unix(0, 0)
	)

	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()

		return now
	}

	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()

		now = now.Add(d)
	}

	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:             5,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: 3 * time.Second,
	}, sampler, nil,
		transcodelimiter.WithNow(clock),
		transcodelimiter.WithPollInterval(time.Millisecond),
	)
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	_, err = lim.ReserveSlot(t.Context(), nil)
	require.NoError(t, err)
	lim.MarkSlotUsed(nil)

	resCh := make(chan error, 1)

	go func() {
		_, reserveErr := lim.ReserveSlot(t.Context(), nil)
		resCh <- reserveErr
	}()

	// Cooldown not yet elapsed → second reservation must still be blocked.
	select {
	case err := <-resCh:
		t.Fatalf("second ReserveSlot returned before cooldown elapsed: err=%v", err)
	case <-time.After(20 * time.Millisecond):
	}

	advance(3 * time.Second)

	select {
	case err := <-resCh:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("ReserveSlot did not unblock after cooldown elapsed")
	}
}

func TestLimiterStaticCapOnlyMode(t *testing.T) {
	sampler := failedSampler()
	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:             2,
		GPUThreshold:          0.8,
		PostAdmissionCooldown: 30 * time.Second,
	}, sampler, nil, transcodelimiter.WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	for index := 0; index < 2; index++ {
		permit, reserveErr := lim.ReserveSlot(t.Context(), nil)
		require.NoErrorf(t, reserveErr, "reservation %d", index)
		require.NotNil(t, permit)
		lim.MarkSlotUsed(nil)
	}

	resCh := make(chan error, 1)

	go func() {
		_, reserveErr := lim.ReserveSlot(t.Context(), nil)
		resCh <- reserveErr
	}()

	select {
	case err := <-resCh:
		t.Fatalf("expected reservation to block at cap in fallback mode, got err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	lim.ReleaseSlot(nil)

	select {
	case err := <-resCh:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("ReserveSlot did not unblock after release in fallback mode")
	}
}

func TestLimiterContextCancellationReturnsError(t *testing.T) {
	sampler := newFakeSampler(0.95)
	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:    5,
		GPUThreshold: 0.8,
	}, sampler, nil, transcodelimiter.WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	ctx, cancel := context.WithCancel(t.Context())

	type result struct {
		permit *worker.SlotPermit
		err    error
	}

	resCh := make(chan result, 1)

	go func() {
		permit, reserveErr := lim.ReserveSlot(ctx, nil)
		resCh <- result{permit: permit, err: reserveErr}
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case r := <-resCh:
		require.Error(t, r.err)
		assert.Nil(t, r.permit)
		assert.True(t, errors.Is(r.err, context.Canceled),
			"expected context.Canceled, got %v", r.err)
	case <-time.After(time.Second):
		t.Fatal("ReserveSlot did not return after context cancellation")
	}
}

func TestLimiterFallbackTransitionFlipsAdmissionModeMetric(t *testing.T) {
	sampler := newFakeSampler(0.1)
	reg := prometheus.NewRegistry()

	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:    5,
		GPUThreshold: 0.8,
	}, sampler, reg, transcodelimiter.WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	require.InDelta(t, 1.0, admissionModeValue(t, reg, "probe"), 0.0001)
	require.InDelta(t, 0.0, admissionModeValue(t, reg, "static"), 0.0001)

	sampler.fail()

	require.Eventually(t, func() bool {
		return admissionModeValue(t, reg, "static") == 1
	}, time.Second, time.Millisecond, "admission_mode{mode=static} did not flip to 1")

	require.InDelta(t, 0.0, admissionModeValue(t, reg, "probe"), 0.0001)
	require.InDelta(t, 1.0, admissionModeValue(t, reg, "static"), 0.0001)
}

func TestLimiterStartsInStaticModeWhenSamplerAlreadyFailed(t *testing.T) {
	sampler := failedSampler()
	reg := prometheus.NewRegistry()

	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:    5,
		GPUThreshold: 0.8,
	}, sampler, reg)
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	require.InDelta(t, 0.0, admissionModeValue(t, reg, "probe"), 0.0001)
	require.InDelta(t, 1.0, admissionModeValue(t, reg, "static"), 0.0001)
}

func TestLimiterTryReserveSlot(t *testing.T) {
	sampler := newFakeSampler(0.1)
	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:    1,
		GPUThreshold: 0.8,
	}, sampler, nil)
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	permit := lim.TryReserveSlot(nil)
	require.NotNil(t, permit, "TryReserveSlot should admit when below threshold and under cap")

	lim.MarkSlotUsed(nil)
	require.Nil(t, lim.TryReserveSlot(nil), "TryReserveSlot should refuse when at cap")
}

func TestLimiterTryReserveSlotRefusesAboveThreshold(t *testing.T) {
	sampler := newFakeSampler(0.95)
	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:    5,
		GPUThreshold: 0.8,
	}, sampler, nil)
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	require.Nil(t, lim.TryReserveSlot(nil))
}

func TestLimiterMaxSlotsReturnsStaticCap(t *testing.T) {
	sampler := newFakeSampler(0)
	lim, err := transcodelimiter.New(transcodelimiter.Config{StaticCap: 7}, sampler, nil)
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	require.Equal(t, 7, lim.MaxSlots())
}

func TestLimiterDefaultsApplied(t *testing.T) {
	sampler := newFakeSampler(0)
	lim, err := transcodelimiter.New(transcodelimiter.Config{}, sampler, nil)
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	// Default static_cap=5; gpu_threshold=0.8; cooldown=3s.
	require.Equal(t, 5, lim.MaxSlots())
}

func TestLimiterRequiresSampler(t *testing.T) {
	_, err := transcodelimiter.New(transcodelimiter.Config{}, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sampler")
}

func TestLimiterMetricsSurfaceIncludesAllFour(t *testing.T) {
	sampler := newFakeSampler(0.42)
	reg := prometheus.NewRegistry()

	lim, err := transcodelimiter.New(transcodelimiter.Config{StaticCap: 3, GPUThreshold: 0.8}, sampler, reg)
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	families, err := reg.Gather()
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, family := range families {
		names[family.GetName()] = true
	}

	for _, expected := range []string{
		"media_worker_transcode_slots_in_flight",
		"media_worker_transcode_slots_blocked_seconds_total",
		"media_worker_transcode_load_utilization",
		"media_worker_transcode_admission_mode",
	} {
		assert.True(t, names[expected], "expected metric %s not registered", expected)
	}

	require.InDelta(t, 0.42, gaugeValueByName(t, families, "media_worker_transcode_load_utilization"), 0.0001)
}

func TestLimiterBlockedSecondsIncreasesUnderContention(t *testing.T) {
	sampler := newFakeSampler(0.95)
	reg := prometheus.NewRegistry()

	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:    5,
		GPUThreshold: 0.8,
	}, sampler, reg, transcodelimiter.WithPollInterval(time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	ctx, cancel := context.WithCancel(t.Context())
	resCh := make(chan struct{}, 1)

	go func() {
		_, _ = lim.ReserveSlot(ctx, nil)

		resCh <- struct{}{}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-resCh

	require.Greater(t, counterValueByName(t, reg, "media_worker_transcode_slots_blocked_seconds_total"), 0.0,
		"blocked_seconds_total should be positive after ctx cancellation while blocked")
}

func TestLimiterCloseIsIdempotent(t *testing.T) {
	sampler := newFakeSampler(0)
	lim, err := transcodelimiter.New(transcodelimiter.Config{}, sampler, nil)
	require.NoError(t, err)

	lim.Close()
	lim.Close() // must not panic
}

// ---- helpers ----

func gaugeValueByName(t *testing.T, families []*dto.MetricFamily, name string) float64 {
	t.Helper()

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, metric := range family.GetMetric() {
			if gauge := metric.GetGauge(); gauge != nil {
				return gauge.GetValue()
			}
		}
	}

	t.Fatalf("metric %s not found", name)

	return 0
}

func counterValueByName(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, metric := range family.GetMetric() {
			if counter := metric.GetCounter(); counter != nil {
				return counter.GetValue()
			}
		}
	}

	t.Fatalf("metric %s not found", name)

	return 0
}

func admissionModeValue(t *testing.T, reg *prometheus.Registry, mode string) float64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != "media_worker_transcode_admission_mode" {
			continue
		}

		for _, metric := range family.GetMetric() {
			matched := false

			for _, label := range metric.GetLabel() {
				if label.GetName() == "mode" && label.GetValue() == mode {
					matched = true
					break
				}
			}

			if !matched {
				continue
			}

			return metric.GetGauge().GetValue()
		}
	}

	t.Fatalf("media_worker_transcode_admission_mode{mode=%q} not found", mode)

	return 0
}
