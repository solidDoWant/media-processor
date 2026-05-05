//go:build linux

package loadprobe

import (
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCgroupFiles(t *testing.T, root, cpuMax, cpuStat string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.max"), []byte(cpuMax), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.stat"), []byte(cpuStat), 0o644))
}

func TestNewCgroupProbe_Unconstrained(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, "max 100000\n", "usage_usec 0\n")

	_, err := NewCgroupProbe(CgroupOptions{CgroupRoot: root})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCgroupUnconstrained)
}

func TestNewCgroupProbe_MalformedCPUMax(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, "garbage\n", "usage_usec 0\n")

	_, err := NewCgroupProbe(CgroupOptions{CgroupRoot: root})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected cpu.max format")
}

func TestNewCgroupProbe_PeriodZero(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, "100000 0\n", "usage_usec 0\n")

	_, err := NewCgroupProbe(CgroupOptions{CgroupRoot: root})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "period is 0")
}

func TestNewCgroupProbe_MissingCPUMax(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.stat"), []byte("usage_usec 0\n"), 0o644))

	_, err := NewCgroupProbe(CgroupOptions{CgroupRoot: root})
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestNewCgroupProbe_MissingCPUStat(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.max"), []byte("100000 100000\n"), 0o644))

	_, err := NewCgroupProbe(CgroupOptions{CgroupRoot: root})
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestCgroupProbe_FirstSampleSeedsAndReturnsZero(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, "100000 100000\n", "usage_usec 0\n")

	clock := newFakeClock(time.Unix(0, 0))
	probe, err := newCgroupProbe(CgroupOptions{CgroupRoot: root}, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	v, err := probe.Sample(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0.0, v)
}

func TestCgroupProbe_QuotaSaturation(t *testing.T) {
	tests := []struct {
		name      string
		cpuMax    string // "<quota> <period>"
		usageUsec uint64 // delta between first and second sample
		wallUs    int64  // wall time elapsed between samples (microseconds)
		want      float64
	}{
		{
			name:      "single_cpu_fully_used",
			cpuMax:    "100000 100000\n",
			usageUsec: 1_000_000, // 1 cpu-second
			wallUs:    1_000_000, // 1 wall-second
			want:      1.0,
		},
		{
			name:      "single_cpu_half_used",
			cpuMax:    "100000 100000\n",
			usageUsec: 500_000,
			wallUs:    1_000_000,
			want:      0.5,
		},
		{
			name:      "four_cpus_quota_one_cpu_used",
			cpuMax:    "400000 100000\n",
			usageUsec: 1_000_000,
			wallUs:    1_000_000,
			want:      0.25,
		},
		{
			name:      "four_cpus_quota_all_used",
			cpuMax:    "400000 100000\n",
			usageUsec: 4_000_000,
			wallUs:    1_000_000,
			want:      1.0,
		},
		{
			// Probe contract is [0, 1] — over-quota readings clamp at the
			// probe boundary so callers don't have to special-case >1.
			name:      "over_quota_clamps_to_one",
			cpuMax:    "100000 100000\n",
			usageUsec: 2_000_000,
			wallUs:    1_000_000,
			want:      1.0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeCgroupFiles(t, root, test.cpuMax, "usage_usec 0\n")

			clock := newFakeClock(time.Unix(0, 0))
			probe, err := newCgroupProbe(CgroupOptions{CgroupRoot: root}, clock.Now)
			require.NoError(t, err)
			t.Cleanup(func() { _ = probe.Close() })

			// Seed sample.
			_, err = probe.Sample(t.Context())
			require.NoError(t, err)

			// Bump usage and wall time, then sample again.
			require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.stat"),
				[]byte("usage_usec "+strconv.FormatUint(test.usageUsec, 10)+"\n"), 0o644))
			clock.Advance(time.Duration(test.wallUs) * time.Microsecond)

			v, err := probe.Sample(t.Context())
			require.NoError(t, err)
			assert.InDelta(t, test.want, v, 1e-9)
		})
	}
}

func TestCgroupProbe_NonMonotonicUsageReturnsZero(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, "100000 100000\n", "usage_usec 1000000\n")

	clock := newFakeClock(time.Unix(0, 0))
	probe, err := newCgroupProbe(CgroupOptions{CgroupRoot: root}, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	// Seed at usage_usec=1_000_000.
	_, err = probe.Sample(t.Context())
	require.NoError(t, err)

	// Counter regresses (e.g. unexpected reset). The probe must not underflow
	// uint64 — return 0 instead.
	require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.stat"),
		[]byte("usage_usec 100000\n"), 0o644))
	clock.Advance(time.Second)

	v, err := probe.Sample(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0.0, v)
}

func TestCgroupProbe_RisesUnderUsage(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, "100000 100000\n", "usage_usec 0\n")

	clock := newFakeClock(time.Unix(0, 0))
	probe, err := newCgroupProbe(CgroupOptions{CgroupRoot: root}, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	// Seed.
	_, err = probe.Sample(t.Context())
	require.NoError(t, err)

	// Each sample's usage rises by at least 200_000 usec over a 1-second
	// wall window — every reading must therefore be positive.
	for _, usage := range []uint64{200_000, 600_000, 1_000_000} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "cpu.stat"),
			[]byte("usage_usec "+strconv.FormatUint(usage, 10)+"\n"), 0o644))
		clock.Advance(time.Second)

		v, err := probe.Sample(t.Context())
		require.NoError(t, err)
		assert.Greater(t, v, 0.0)
	}
}

func TestCgroupProbe_ReadUsageUsec_MissingField(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, "100000 100000\n", "user_usec 100\nsystem_usec 50\n")

	clock := newFakeClock(time.Unix(0, 0))
	probe, err := newCgroupProbe(CgroupOptions{CgroupRoot: root}, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	_, err = probe.Sample(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usage_usec not found")
}

// fakeClock is a monotonic test clock that advances only when Advance is
// called. Avoids real-time sleeps in cgroup math tests.
type fakeClock struct {
	nanos atomic.Int64
}

func newFakeClock(start time.Time) *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(start.UnixNano())

	return c
}

func (c *fakeClock) Now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *fakeClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }
