//go:build linux

package loadprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pmuFixture describes a single PMU directory for buildSysfs.
type pmuFixture struct {
	name        string         // e.g. "i915" or "i915_0000_03_00.0"
	pmuType     uint64         // value written to <pmu>/type
	cpumask     string         // value written to <pmu>/cpumask (omitted when empty)
	vcsConfigs  map[string]any // event name → config string content; if uint64 we wrap in "config=0x..."
	extraEvents []string       // event names to create empty (e.g. "rcs0-busy")
}

// renderNodeFixture describes a /sys/class/drm/<node>/device → BDF mapping.
type renderNodeFixture struct {
	node string // e.g. "renderD128"
	bdf  string // e.g. "0000:00:02.0"
}

// buildSysfs lays out a fake sysfs under t.TempDir() and returns the root.
// The result mirrors a real host's layout closely enough that the probe's
// resolver and parser exercise the same code paths.
func buildSysfs(t *testing.T, nodes []renderNodeFixture, pmus []pmuFixture) string {
	t.Helper()
	root := t.TempDir()

	for _, node := range nodes {
		drmDir := filepath.Join(root, "sys/class/drm", node.node)
		require.NoError(t, os.MkdirAll(drmDir, 0o755))

		deviceTargetDir := filepath.Join(root, "sys/devices/pci0000:00", node.bdf)
		require.NoError(t, os.MkdirAll(deviceTargetDir, 0o755))

		require.NoError(t, os.Symlink(deviceTargetDir, filepath.Join(drmDir, "device")))
	}

	for _, pmu := range pmus {
		pmuDir := filepath.Join(root, "sys/bus/event_source/devices", pmu.name)
		require.NoError(t, os.MkdirAll(pmuDir, 0o755))

		require.NoError(t, os.WriteFile(filepath.Join(pmuDir, "type"),
			[]byte(strconv.FormatUint(pmu.pmuType, 10)+"\n"), 0o644))

		if pmu.cpumask != "" {
			require.NoError(t, os.WriteFile(filepath.Join(pmuDir, "cpumask"),
				[]byte(pmu.cpumask+"\n"), 0o644))
		}

		eventsDir := filepath.Join(pmuDir, "events")
		require.NoError(t, os.MkdirAll(eventsDir, 0o755))

		for vcsConfigName, vcsConfigRaw := range pmu.vcsConfigs {
			var content string

			switch vcsConfigValue := vcsConfigRaw.(type) {
			case uint64:
				content = "config=0x" + strconv.FormatUint(vcsConfigValue, 16) + "\n"
			case string:
				content = vcsConfigValue
			default:
				require.FailNowf(t, "buildSysfs: unsupported event content type", "type=%T", vcsConfigRaw)
			}

			require.NoError(t, os.WriteFile(filepath.Join(eventsDir, vcsConfigName),
				[]byte(content), 0o644))
			// Companion .unit file: present on real hosts but irrelevant to parsing.
			require.NoError(t, os.WriteFile(filepath.Join(eventsDir, vcsConfigName+".unit"),
				[]byte("ns\n"), 0o644))
		}

		for _, extraEvent := range pmu.extraEvents {
			require.NoError(t, os.WriteFile(filepath.Join(eventsDir, extraEvent),
				[]byte("config=0x100\n"), 0o644))
		}
	}

	return root
}

func TestResolveI915PMU_MultiGPU_MatchesVerifiedHostLayout(t *testing.T) {
	// Mirrors the verified host: iGPU at 0000:00:02.0 registered as bare
	// "i915", dGPU at 0000:03:00.0 registered as "i915_0000_03_00.0".
	root := buildSysfs(t,
		[]renderNodeFixture{
			{node: "renderD128", bdf: "0000:00:02.0"},
			{node: "renderD129", bdf: "0000:03:00.0"},
		},
		[]pmuFixture{
			{name: "i915", pmuType: 31},
			{name: "i915_0000_03_00.0", pmuType: 32},
		},
	)

	tests := []struct {
		device     string
		wantSuffix string
	}{
		{"/dev/dri/renderD128", "i915"},
		{"/dev/dri/renderD129", "i915_0000_03_00.0"},
	}
	for _, test := range tests {
		t.Run(test.device, func(t *testing.T) {
			pmuDir, err := resolveI915PMU(test.device, root)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(root, "sys/bus/event_source/devices", test.wantSuffix), pmuDir)
		})
	}
}

func TestResolveI915PMU_SingleGPU_FallsBackToBareI915(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
		[]pmuFixture{{name: "i915", pmuType: 31}},
	)

	pmuDir, err := resolveI915PMU("/dev/dri/renderD128", root)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "sys/bus/event_source/devices/i915"), pmuDir)
}

func TestResolveI915PMU_NoMatchingPMU(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
		nil,
	)

	_, err := resolveI915PMU("/dev/dri/renderD128", root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no i915 PMU found")
	assert.Contains(t, err.Error(), "i915_0000_00_02.0")
}

func TestResolveI915PMU_MissingDeviceSymlink(t *testing.T) {
	root := t.TempDir()

	_, err := resolveI915PMU("/dev/dri/renderD128", root)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestResolveI915PMU_InvalidDevicePath(t *testing.T) {
	_, err := resolveI915PMU("/", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device path")
}

func TestReadVCSEngineConfigs_TwoEngines(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD129", bdf: "0000:03:00.0"}},
		[]pmuFixture{{
			name:    "i915_0000_03_00.0",
			pmuType: 32,
			vcsConfigs: map[string]any{
				"vcs0-busy": uint64(0x2000),
				"vcs1-busy": uint64(0x2010),
			},
			extraEvents: []string{"rcs0-busy", "vecs0-busy"}, // must be filtered out
		}},
	)

	configs, err := readVCSEngineConfigs(filepath.Join(root, "sys/bus/event_source/devices/i915_0000_03_00.0"))
	require.NoError(t, err)
	assert.Equal(t, []uint64{0x2000, 0x2010}, configs)
}

func TestReadVCSEngineConfigs_NoVCSEvents(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
		[]pmuFixture{{
			name:        "i915",
			pmuType:     31,
			extraEvents: []string{"rcs0-busy", "bcs0-busy"},
		}},
	)

	configs, err := readVCSEngineConfigs(filepath.Join(root, "sys/bus/event_source/devices/i915"))
	require.NoError(t, err)
	assert.Empty(t, configs)
}

func TestReadVCSEngineConfigs_MalformedSpec(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
		[]pmuFixture{{
			name:    "i915",
			pmuType: 31,
			vcsConfigs: map[string]any{
				"vcs0-busy": "garbage\n",
			},
		}},
	)

	_, err := readVCSEngineConfigs(filepath.Join(root, "sys/bus/event_source/devices/i915"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed spec")
}

func TestReadVCSEngineConfigs_MissingEventsDir(t *testing.T) {
	pmuDir := t.TempDir()
	_, err := readVCSEngineConfigs(pmuDir)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadPMUCPU(t *testing.T) {
	tests := []struct {
		name    string
		content string
		write   bool
		want    int
		errFunc require.ErrorAssertionFunc
	}{
		{name: "missing_file", write: false, want: 0},
		{name: "single_cpu", content: "0\n", write: true, want: 0},
		{name: "specific_cpu", content: "4\n", write: true, want: 4},
		{name: "range", content: "0-3\n", write: true, want: 0},
		{name: "list", content: "2,4-7\n", write: true, want: 2},
		{name: "empty", content: "\n", write: true, want: 0},
		{name: "invalid", content: "garbage\n", write: true, errFunc: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errFunc == nil {
				test.errFunc = require.NoError
			}

			path := filepath.Join(t.TempDir(), "cpumask")
			if test.write {
				require.NoError(t, os.WriteFile(path, []byte(test.content), 0o644))
			}

			got, err := readPMUCPU(path)
			test.errFunc(t, err)

			if err == nil {
				assert.Equal(t, test.want, got)
			}
		})
	}
}

func TestNewIntelProbe_OpensOneFDPerEngine(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD129", bdf: "0000:03:00.0"}},
		[]pmuFixture{{
			name:    "i915_0000_03_00.0",
			pmuType: 32,
			cpumask: "0",
			vcsConfigs: map[string]any{
				"vcs0-busy": uint64(0x2000),
				"vcs1-busy": uint64(0x2010),
			},
		}},
	)

	var openCalls []openCall

	syscalls := intelSyscalls{
		open: func(pmuType uint32, config uint64, cpu int) (int, error) {
			openCalls = append(openCalls, openCall{pmuType: pmuType, config: config, cpu: cpu})
			return 100 + len(openCalls), nil
		},
		read:  func(int) (uint64, error) { return 0, nil },
		close: func(int) error { return nil },
	}

	probe, err := newIntelProbe("/dev/dri/renderD129", IntelOptions{SysRoot: root}, syscalls)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	require.Len(t, openCalls, 2)
	assert.Equal(t, uint32(32), openCalls[0].pmuType)
	assert.Equal(t, uint64(0x2000), openCalls[0].config)
	assert.Equal(t, 0, openCalls[0].cpu)
	assert.Equal(t, uint64(0x2010), openCalls[1].config)
}

func TestNewIntelProbe_PerfOpenErrorsPropagate(t *testing.T) {
	// Each case represents an errno the kernel can return from
	// perf_event_open that callers (the supplier in #199 group 6) need to
	// observe verbatim:
	//   - EACCES: paranoid > 1 with no CAP_PERFMON.
	//   - ENOTSUP: kernel does not expose the requested PMU capability.
	tests := []struct {
		name    string
		openErr error
	}{
		{name: "eacces", openErr: syscall.EACCES},
		{name: "enotsup", openErr: syscall.ENOTSUP},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := buildSysfs(t,
				[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
				[]pmuFixture{{
					name:       "i915",
					pmuType:    31,
					cpumask:    "0",
					vcsConfigs: map[string]any{"vcs0-busy": uint64(0x2000)},
				}},
			)

			syscalls := intelSyscalls{
				open: func(uint32, uint64, int) (int, error) { return -1, test.openErr },
				read: func(int) (uint64, error) { return 0, nil },
				close: func(int) error {
					require.FailNow(t, "close must not be called when no fds were opened")
					return nil
				},
			}

			_, err := newIntelProbe("/dev/dri/renderD128", IntelOptions{SysRoot: root}, syscalls)
			require.Error(t, err)
			assert.ErrorIs(t, err, test.openErr)
			assert.Contains(t, err.Error(), "perf_event_open")
		})
	}
}

func TestNewIntelProbe_SecondOpenFailureClosesFirstFD(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD129", bdf: "0000:03:00.0"}},
		[]pmuFixture{{
			name:    "i915_0000_03_00.0",
			pmuType: 32,
			cpumask: "0",
			vcsConfigs: map[string]any{
				"vcs0-busy": uint64(0x2000),
				"vcs1-busy": uint64(0x2010),
			},
		}},
	)

	openCount := 0
	closedFDs := []int{}
	syscalls := intelSyscalls{
		open: func(uint32, uint64, int) (int, error) {
			openCount++
			if openCount == 1 {
				return 200, nil
			}

			return -1, syscall.ENOENT
		},
		read: func(int) (uint64, error) { return 0, nil },
		close: func(fd int) error {
			closedFDs = append(closedFDs, fd)
			return nil
		},
	}

	_, err := newIntelProbe("/dev/dri/renderD129", IntelOptions{SysRoot: root}, syscalls)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ENOENT)
	assert.Equal(t, []int{200}, closedFDs, "constructor must close the fd it already opened on partial failure")
}

func TestIntelProbe_SampleRisesUnderLoad(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD129", bdf: "0000:03:00.0"}},
		[]pmuFixture{{
			name:       "i915_0000_03_00.0",
			pmuType:    32,
			cpumask:    "0",
			vcsConfigs: map[string]any{"vcs0-busy": uint64(0x2000)},
		}},
	)

	// counter value rises by 800ms-of-busy per call after the seed.
	counter := uint64(0)
	syscalls := intelSyscalls{
		open: func(uint32, uint64, int) (int, error) { return 1, nil },
		read: func(int) (uint64, error) {
			value := counter
			counter += 800_000_000

			return value, nil
		},
		close: func(int) error { return nil },
	}

	probe, err := newIntelProbe("/dev/dri/renderD129", IntelOptions{SysRoot: root}, syscalls)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	first, err := probe.Sample(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0.0, first, "first sample seeds reference state and returns 0")

	// The fake counter rises by 800ms-of-busy per call. Once enough wall
	// time has elapsed the sample should be positive; busy time well above
	// wall time must clamp to 1.0 rather than overshoot the probe contract.
	deadline, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	for deadline.Err() == nil {
		v, err := probe.Sample(deadline)
		if errors.Is(err, context.DeadlineExceeded) {
			break
		}

		require.NoError(t, err)
		require.GreaterOrEqual(t, v, 0.0, "Sample produced a negative value")
		require.LessOrEqual(t, v, 1.0, "Sample exceeded probe contract upper bound")

		if v > 0 {
			return
		}
	}

	require.FailNow(t, "Sample did not produce a positive value within deadline")
}

func TestIntelProbe_SampleNonMonotonicCounterReturnsZero(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
		[]pmuFixture{{
			name:       "i915",
			pmuType:    31,
			cpumask:    "0",
			vcsConfigs: map[string]any{"vcs0-busy": uint64(0x2000)},
		}},
	)

	// First read: counter at 1_000_000_000 ns. Second read: counter has
	// regressed (e.g. PMU reset). The probe must not underflow uint64.
	values := []uint64{1_000_000_000, 100_000_000}
	idx := 0
	syscalls := intelSyscalls{
		open: func(uint32, uint64, int) (int, error) { return 1, nil },
		read: func(int) (uint64, error) {
			v := values[idx%len(values)]
			idx++

			return v, nil
		},
		close: func(int) error { return nil },
	}

	probe, err := newIntelProbe("/dev/dri/renderD128", IntelOptions{SysRoot: root}, syscalls)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	// Seed.
	_, err = probe.Sample(t.Context())
	require.NoError(t, err)

	// Sleep to advance wall time so the divisor isn't zero; the regressed
	// counter must produce 0, not a wrap-around utilization.
	time.Sleep(2 * time.Millisecond)

	v, err := probe.Sample(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0.0, v)
}

func TestIntelProbe_SampleAfterCloseReturnsError(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
		[]pmuFixture{{
			name:       "i915",
			pmuType:    31,
			cpumask:    "0",
			vcsConfigs: map[string]any{"vcs0-busy": uint64(0x2000)},
		}},
	)

	closeCalls := 0
	syscalls := intelSyscalls{
		open: func(uint32, uint64, int) (int, error) { return 50, nil },
		read: func(int) (uint64, error) { return 0, nil },
		close: func(int) error {
			closeCalls++

			return nil
		},
	}

	probe, err := newIntelProbe("/dev/dri/renderD128", IntelOptions{SysRoot: root}, syscalls)
	require.NoError(t, err)

	require.NoError(t, probe.Close())
	require.NoError(t, probe.Close()) // idempotent

	_, err = probe.Sample(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, closeCalls, "Close must close the underlying fd exactly once across repeated calls")
}

func TestIntelProbe_SampleReadErrorPropagates(t *testing.T) {
	root := buildSysfs(t,
		[]renderNodeFixture{{node: "renderD128", bdf: "0000:00:02.0"}},
		[]pmuFixture{{
			name:       "i915",
			pmuType:    31,
			cpumask:    "0",
			vcsConfigs: map[string]any{"vcs0-busy": uint64(0x2000)},
		}},
	)

	syscalls := intelSyscalls{
		open:  func(uint32, uint64, int) (int, error) { return 50, nil },
		read:  func(int) (uint64, error) { return 0, errors.New("EIO") },
		close: func(int) error { return nil },
	}

	probe, err := newIntelProbe("/dev/dri/renderD128", IntelOptions{SysRoot: root}, syscalls)
	require.NoError(t, err)
	t.Cleanup(func() { _ = probe.Close() })

	_, err = probe.Sample(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read perf fd")
}

type openCall struct {
	pmuType uint32
	config  uint64
	cpu     int
}
