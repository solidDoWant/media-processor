package loadprobe_test

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/loadprobe"
)

type sampleResult struct {
	value float64
	err   error
}

type fakeProbe struct {
	samples   chan sampleResult
	callCount atomic.Int32
	closed    atomic.Bool
}

func newFakeProbe(buffered int) *fakeProbe {
	return &fakeProbe{samples: make(chan sampleResult, buffered)}
}

func (f *fakeProbe) push(v float64, err error) {
	f.samples <- sampleResult{value: v, err: err}
}

func (f *fakeProbe) Sample(ctx context.Context) (float64, error) {
	f.callCount.Add(1)

	select {
	case s := <-f.samples:
		return s.value, s.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (f *fakeProbe) Close() error {
	f.closed.Store(true)
	return nil
}

func TestSampler_RisingEWMA(t *testing.T) {
	fp := newFakeProbe(10)
	for range 8 {
		fp.push(1.0, nil)
	}

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        time.Millisecond,
		SmoothingWindow: 5,
	})

	t.Cleanup(func() { _ = s.Close() })
	s.Start(t.Context())

	require.Eventually(t, func() bool {
		return s.Value() > 0.5
	}, time.Second, 5*time.Millisecond, "Value did not rise toward 1.0")
	assert.LessOrEqual(t, s.Value(), 1.0)
}

func TestSampler_FallingEWMA(t *testing.T) {
	fp := newFakeProbe(20)
	// Saturate first.
	for range 6 {
		fp.push(1.0, nil)
	}
	// Then drop to zero.
	for range 14 {
		fp.push(0.0, nil)
	}

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        time.Millisecond,
		SmoothingWindow: 5,
	})

	t.Cleanup(func() { _ = s.Close() })
	s.Start(t.Context())

	require.Eventually(t, func() bool {
		return s.Value() < 0.1
	}, time.Second, 5*time.Millisecond, "Value did not fall toward 0")
}

func TestSampler_BoundsClamping(t *testing.T) {
	fp := newFakeProbe(20)
	// Out-of-range probe outputs are clamped, not rejected.
	for range 5 {
		fp.push(-0.1, nil)
	}

	for range 5 {
		fp.push(1.5, nil)
	}

	for range 10 {
		fp.push(0.5, nil)
	}

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        time.Millisecond,
		SmoothingWindow: 3,
	})

	t.Cleanup(func() { _ = s.Close() })
	s.Start(t.Context())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v := s.Value()
		require.GreaterOrEqual(t, v, 0.0, "Value below 0 — clamp failed")
		require.LessOrEqual(t, v, 1.0, "Value above 1 — clamp failed")
		time.Sleep(2 * time.Millisecond)
	}

	select {
	case <-s.FailedC():
		t.Fatal("out-of-range probe outputs must not trigger fallback")
	default:
	}
}

func TestSampler_NaNProbeOutputClamped(t *testing.T) {
	fp := newFakeProbe(5)
	fp.push(math.NaN(), nil)

	for range 4 {
		fp.push(0.5, nil)
	}

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        time.Millisecond,
		SmoothingWindow: 3,
	})

	t.Cleanup(func() { _ = s.Close() })
	s.Start(t.Context())

	require.Eventually(t, func() bool {
		v := s.Value()
		return v > 0
	}, time.Second, 5*time.Millisecond)
}

func TestSampler_FailedConstructor(t *testing.T) {
	cause := errors.New("perf_event_open: permission denied")
	s := loadprobe.Failed(cause, slog.New(slog.DiscardHandler))

	t.Cleanup(func() { _ = s.Close() })

	select {
	case <-s.FailedC():
	default:
		t.Fatal("FailedC must be closed immediately on a Failed-constructed sampler")
	}

	assert.ErrorIs(t, s.FailureReason(), cause)
	assert.Equal(t, 0.0, s.Value())
}

func TestSampler_MidStreamFailureClosesChannel(t *testing.T) {
	fp := newFakeProbe(2)
	fp.push(0.5, nil)
	fp.push(0, errors.New("read perf fd: input/output error"))

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        time.Millisecond,
		SmoothingWindow: 3,
	})

	t.Cleanup(func() { _ = s.Close() })
	s.Start(t.Context())

	select {
	case <-s.FailedC():
	case <-time.After(time.Second):
		t.Fatal("FailedC did not close after probe sample failure")
	}

	require.Error(t, s.FailureReason())
	assert.Contains(t, s.FailureReason().Error(), "input/output error")

	// Wait one sample interval to confirm no further Sample calls happen.
	beforeCount := fp.callCount.Load()

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, beforeCount, fp.callCount.Load(), "probe Sample called after fallback")
}

func TestSampler_ContextCancellationDoesNotFail(t *testing.T) {
	fp := newFakeProbe(1)
	fp.push(0.5, nil)

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        50 * time.Millisecond,
		SmoothingWindow: 3,
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { _ = s.Close() })
	s.Start(ctx)

	require.Eventually(t, func() bool {
		return s.Value() > 0
	}, time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, s.Close())

	select {
	case <-s.FailedC():
		t.Fatal("FailedC closed on context cancellation — only probe errors should fail")
	default:
	}

	assert.NoError(t, s.FailureReason())
}

func TestSampler_CloseClosesProbe(t *testing.T) {
	fp := newFakeProbe(1)
	fp.push(0.5, nil)

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        50 * time.Millisecond,
		SmoothingWindow: 3,
	})
	s.Start(t.Context())

	require.Eventually(t, func() bool { return s.Value() > 0 }, time.Second, 5*time.Millisecond)
	require.NoError(t, s.Close())
	assert.True(t, fp.closed.Load(), "Close on Sampler did not close the underlying probe")

	// Idempotent.
	require.NoError(t, s.Close())
}

func TestSampler_StartIsIdempotent(t *testing.T) {
	fp := newFakeProbe(5)
	for range 5 {
		fp.push(0.5, nil)
	}

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{
		Interval:        time.Millisecond,
		SmoothingWindow: 3,
	})

	t.Cleanup(func() { _ = s.Close() })

	s.Start(t.Context())
	s.Start(t.Context()) // second call must be a no-op

	require.Eventually(t, func() bool { return s.Value() > 0 }, time.Second, 5*time.Millisecond)
}

func TestSampler_DefaultConfigValues(t *testing.T) {
	// A zero SamplerConfig must apply defaults so callers don't have to
	// special-case empty config.
	fp := newFakeProbe(1)
	fp.push(0.5, nil)

	s := loadprobe.NewSampler(fp, loadprobe.SamplerConfig{})

	t.Cleanup(func() { _ = s.Close() })
	s.Start(t.Context())

	require.Eventually(t, func() bool { return s.Value() > 0 }, 2*time.Second, 50*time.Millisecond)
}
