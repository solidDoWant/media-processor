//go:build integration && linux

package loadprobe_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/loadprobe"
)

// TestCgroupProbe_Integration_RisesUnderCPULoad burns CPU in a goroutine
// pool sized to the container's CPU quota and verifies the cgroup probe
// reports a non-trivial utilization.
//
// Skips with a specific reason when:
//   - cgroup v2 is not mounted (no /sys/fs/cgroup/cgroup.controllers).
//   - /proc/self/cgroup cannot be parsed.
//   - The process's cgroup has no CPU quota (cpu.max is "max ...").
func TestCgroupProbe_Integration_RisesUnderCPULoad(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("cgroup v2 not mounted at /sys/fs/cgroup — skipping; run on a host with a unified cgroup hierarchy")
	}

	cgroupDir, err := readSelfCgroupV2Dir()
	if err != nil {
		t.Skipf("cannot resolve own cgroup v2 directory: %v", err)
	}

	probe, err := loadprobe.NewCgroupProbe(loadprobe.CgroupOptions{CgroupRoot: cgroupDir})
	if err != nil {
		if errors.Is(err, loadprobe.ErrCgroupUnconstrained) {
			t.Skipf("cgroup at %s has no CPU quota — skipping; run inside a container with cpu.max set", cgroupDir)
		}

		t.Fatalf("NewCgroupProbe: %v", err)
	}

	t.Cleanup(func() { _ = probe.Close() })

	// Seed sample.
	_, err = probe.Sample(t.Context())
	require.NoError(t, err)

	// Burn CPU for 500ms across all available threads. We don't use
	// runtime.NumCPU as a strict bound — even a single busy thread will
	// register on a small-quota cgroup.
	stop := atomic.Bool{}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			x := uint64(1)
			for !stop.Load() {
				x = x*1103515245 + 12345
			}

			runtime.KeepAlive(x)
		}()
	}

	time.Sleep(500 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	v, err := probe.Sample(t.Context())
	require.NoError(t, err)
	assert.Greater(t, v, 0.0, "cgroup probe should report non-zero utilization after CPU burn")
}

// readSelfCgroupV2Dir returns the absolute path of the calling process's
// cgroup v2 directory (e.g. "/sys/fs/cgroup/system.slice/foo.scope").
func readSelfCgroupV2Dir() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		// cgroup v2 unified line: "0::<path>"
		rest, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}

		path := strings.TrimSpace(rest)

		return filepath.Join("/sys/fs/cgroup", path), nil
	}

	return "", errors.New("no cgroup v2 unified entry in /proc/self/cgroup")
}
