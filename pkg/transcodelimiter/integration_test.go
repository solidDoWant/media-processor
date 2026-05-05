//go:build integration && gpu

package transcodelimiter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/loadprobe"
	"github.com/solidDoWant/media-processor/pkg/transcodelimiter"
)

// TestLimiterEndToEndWithIntelProbe drives the limiter against the real i915
// PMU. The `gpu` build tag gates compilation: operators with an Intel GPU
// host opt in via `make test-integration-gpu` (or
// `go test -tags=integration,gpu ./pkg/transcodelimiter/...`); all other
// runners never compile this file. Required runtime permission: CAP_PERFMON
// or kernel.perf_event_paranoid <= 1. Skips with a clear message when no
// /dev/dri/renderD* node is present or when perf_event_open returns
// EACCES/ENOTSUP/ENOENT (mirrors the existing pkg/loadprobe i915 integration
// test) so a CI runner that has the build tag on but lacks permission
// reports actionable status instead of a hard failure.
func TestLimiterEndToEndWithIntelProbe(t *testing.T) {
	devicePath := pickI915DevicePath(t)

	probe, err := loadprobe.NewIntelProbe(devicePath, loadprobe.IntelOptions{})
	if err != nil {
		if isExpectedProbeSkip(err) {
			t.Skipf("NewIntelProbe(%s) returned a documented skip error: %v (grant CAP_PERFMON or set kernel.perf_event_paranoid<=1 to run this test)", devicePath, err)
		}

		require.NoError(t, err, "unexpected NewIntelProbe failure")
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
		GPUThreshold:          0.99,
		PostAdmissionCooldown: 50 * time.Millisecond,
	}, sampler, nil, transcodelimiter.WithPollInterval(10*time.Millisecond))
	require.NoError(t, err)

	t.Cleanup(lim.Close)

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
	// capacity. A short context bound is enough to confirm the block.
	blockCtx, blockCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer blockCancel()

	_, err = lim.ReserveSlot(blockCtx, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled),
		"expected context error while at cap, got %v", err)

	lim.ReleaseSlot(nil)

	third, err := lim.ReserveSlot(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, third)
}

// pickI915DevicePath scans /dev/dri for the lowest-numbered render node and
// returns its path. Skips the test when no node is found so a runner without
// a DRM device under the `gpu` tag still reports a useful status instead of
// silently passing or hard-failing.
func pickI915DevicePath(t *testing.T) string {
	t.Helper()

	nodes, err := filepath.Glob("/dev/dri/renderD*")
	require.NoError(t, err)

	if len(nodes) == 0 {
		t.Skip("no /dev/dri/renderD* nodes found; provision an Intel GPU to run this test")
	}

	sort.Strings(nodes)

	return nodes[0]
}

// isExpectedProbeSkip returns true for the documented errors that mean
// "no usable i915 PMU on this host" — EACCES, ENOTSUP, ENOENT, or a missing
// device file — so the test skips with a clear message instead of failing.
// Any other error is a real regression in NewIntelProbe and must fail the
// test.
func isExpectedProbeSkip(err error) bool {
	return errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, os.ErrNotExist)
}
