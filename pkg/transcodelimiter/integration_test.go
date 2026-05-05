//go:build integration

package transcodelimiter_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/loadprobe"
	"github.com/solidDoWant/media-processor/pkg/transcodelimiter"
)

// TestLimiterEndToEndWithIntelProbe drives the limiter against the real i915
// PMU. The test is skipped (with a clear, actionable message) when no GPU is
// available — the host needs an Intel render node and the worker process
// needs CAP_PERFMON or kernel.perf_event_paranoid<=1 for perf_event_open to
// succeed.
//
// The device path is taken from TRANSCODE_LIMITER_TEST_DEVICE_PATH; absence
// of that env var is treated as "no GPU" and skipped. This mirrors the
// pattern used elsewhere for hardware-dependent integration tests.
func TestLimiterEndToEndWithIntelProbe(t *testing.T) {
	devicePath := os.Getenv("TRANSCODE_LIMITER_TEST_DEVICE_PATH")
	if devicePath == "" {
		t.Skip("TRANSCODE_LIMITER_TEST_DEVICE_PATH unset; skipping i915 integration test (set to e.g. /dev/dri/renderD128 on a GPU host)")
	}

	probe, err := loadprobe.NewIntelProbe(devicePath, loadprobe.IntelOptions{})
	if err != nil {
		t.Skipf("NewIntelProbe failed: %v (likely needs CAP_PERFMON or kernel.perf_event_paranoid<=1)", err)
	}

	t.Cleanup(func() { _ = probe.Close() })

	sampler := loadprobe.NewSampler(probe, loadprobe.SamplerConfig{
		Interval:        100 * time.Millisecond,
		SmoothingWindow: 5,
	})
	sampler.Start(t.Context())

	t.Cleanup(func() { _ = sampler.Close() })

	lim, err := transcodelimiter.New(transcodelimiter.Config{
		StaticCap:             2,
		GPUThreshold:          0.99, // permissive — we want admission to succeed when idle
		PostAdmissionCooldown: 50 * time.Millisecond,
	}, sampler, nil, transcodelimiter.WithPollInterval(10*time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

	// Acquire two slots successfully — the GPU is idle so the probe value
	// should remain below 0.99.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	first, err := lim.ReserveSlot(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, first)
	lim.MarkSlotUsed(nil)

	second, err := lim.ReserveSlot(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, second)
	lim.MarkSlotUsed(nil)

	// In-flight equals static cap; the third reservation must block on
	// capacity. Cancel after a short delay and confirm the error.
	blockCtx, blockCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer blockCancel()

	_, err = lim.ReserveSlot(blockCtx, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled),
		"expected context error while at cap, got %v", err)

	// Release one slot; a fresh reservation should succeed.
	lim.ReleaseSlot(nil)

	third, err := lim.ReserveSlot(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, third)
}
