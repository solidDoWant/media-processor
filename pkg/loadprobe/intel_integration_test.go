//go:build integration && linux

package loadprobe_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/loadprobe"
)

// TestIntelProbe_Integration_BindsToRealHardware verifies the probe
// constructs and samples against a real i915 PMU on the host.
//
// The test deliberately covers only initialization and bounded-sampling
// behaviour. Verifying that the value actually rises while a transcode is
// running requires driving an ffmpeg transcode against the device, which
// lives at the worker end-to-end layer where this probe is wired into the
// slot supplier. This integration test verifies only that NewIntelProbe +
// Sample work end-to-end against hardware.
//
// Skips with a specific reason when:
//   - No /dev/dri/renderD* nodes exist (no DRM-capable GPU on the host).
//   - All render nodes are non-i915 or the matching PMU is missing.
//   - perf_event_open returns EACCES/ENOTSUP/ENOENT — typically because
//     kernel.perf_event_paranoid > 1 and the test process lacks CAP_PERFMON.
func TestIntelProbe_Integration_BindsToRealHardware(t *testing.T) {
	nodes, err := filepath.Glob("/dev/dri/renderD*")
	require.NoError(t, err)

	if len(nodes) == 0 {
		t.Skip("no /dev/dri/renderD* nodes — skipping; provision an Intel GPU to run this test")
	}

	sort.Strings(nodes)

	var (
		probe      *loadprobe.IntelProbe
		attempts   []string
		unexpected []string
	)

	for _, node := range nodes {
		p, openErr := loadprobe.NewIntelProbe(node, loadprobe.IntelOptions{})
		if openErr == nil {
			probe = p

			t.Logf("loadprobe: bound to %s", node)

			break
		}

		attempts = append(attempts, node+": "+openErr.Error())

		// Expected skip conditions: EACCES/ENOTSUP/ENOENT from
		// perf_event_open mean the kernel forbids the call or the device is
		// not an i915 render node. Anything else is a real regression in
		// NewIntelProbe and must fail the test rather than silently skip.
		if errors.Is(openErr, syscall.EACCES) ||
			errors.Is(openErr, syscall.ENOTSUP) ||
			errors.Is(openErr, syscall.ENOENT) ||
			errors.Is(openErr, os.ErrNotExist) {
			continue
		}

		unexpected = append(unexpected, node+": "+openErr.Error())
	}

	if probe == nil && len(unexpected) > 0 {
		t.Fatalf("unexpected NewIntelProbe failures (not the documented skip conditions):\n%s", joinLines(unexpected))
	}

	if probe == nil {
		t.Skipf("no usable i915 PMU on this host — skipping; either no i915 device, or kernel.perf_event_paranoid > 1 with no CAP_PERFMON. Attempts:\n%s", joinLines(attempts))
	}

	t.Cleanup(func() { _ = probe.Close() })

	// First sample seeds reference state.
	first, err := probe.Sample(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0.0, first, "first sample must seed reference state and return 0")

	// Sleep briefly so wall-time elapses, then sample again.
	time.Sleep(100 * time.Millisecond)

	v, err := probe.Sample(t.Context())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, v, 0.0, "Sample produced a negative value: %v", v)
	// The probe doesn't clamp — that's the Sampler's job. We only check the
	// raw sample is non-negative; on an idle GPU it should be near 0.
	assert.LessOrEqual(t, v, 8.0, "Sample value implausibly large for an idle GPU: %v", v)
}

func joinLines(s []string) string {
	out := ""
	for _, line := range s {
		out += "  - " + line + "\n"
	}

	return out
}
