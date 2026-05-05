//go:build linux

package loadprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// IntelOptions configures the i915 PMU probe.
type IntelOptions struct {
	// SysRoot is the filesystem root prepended to /sys paths. Defaults to
	// "/" when empty. Used by tests to inject fake sysfs layouts.
	SysRoot string
}

// IntelProbe samples Intel i915 video-engine utilization via perf_event_open
// against the device-specific PMU. Each engine-busy counter increments by
// nanoseconds-of-engine-busy; Sample sums the deltas across the device's
// vcs* engines and divides by elapsed wall time.
type IntelProbe struct {
	fds      []int
	syscalls intelSyscalls

	mu       sync.Mutex
	lastVal  uint64
	lastTime time.Time
	closed   bool
}

// intelSyscalls is the small syscall surface the probe needs. Made
// injectable so unit tests can drive the probe without root or hardware.
type intelSyscalls struct {
	open  func(pmuType uint32, config uint64, cpu int) (int, error)
	read  func(fd int) (uint64, error)
	close func(fd int) error
}

func defaultIntelSyscalls() intelSyscalls {
	return intelSyscalls{
		open: func(pmuType uint32, config uint64, cpu int) (int, error) {
			attr := unix.PerfEventAttr{
				Type:   pmuType,
				Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
				Config: config,
			}

			fd, err := unix.PerfEventOpen(&attr, -1, cpu, -1, 0)
			if err != nil {
				return -1, err
			}

			return fd, nil
		},
		read: func(fd int) (uint64, error) {
			var buf [8]byte

			n, err := unix.Read(fd, buf[:])
			if err != nil {
				return 0, err
			}

			if n != 8 {
				return 0, fmt.Errorf("short read from perf fd: %d bytes", n)
			}

			return binary.LittleEndian.Uint64(buf[:]), nil
		},
		close: unix.Close,
	}
}

// NewIntelProbe binds a probe to the i915 device backing devicePath
// (e.g. "/dev/dri/renderD128"). The probe is bound to the same physical
// device the worker transcodes against — on multi-i915 hosts the matching
// per-device PMU is selected via the device-path → PCI BDF mapping.
//
// Returns an error wrapping the underlying cause when the PMU cannot be
// resolved, the kernel rejects the perf_event_open call (EACCES, ENOTSUPP,
// ENOENT), or no vcs* engines are exposed. Callers can fold the error into
// the same fallback surface as a mid-stream failure via Failed(err, log).
func NewIntelProbe(devicePath string, opts IntelOptions) (*IntelProbe, error) {
	return newIntelProbe(devicePath, opts, defaultIntelSyscalls())
}

func newIntelProbe(devicePath string, opts IntelOptions, syscalls intelSyscalls) (*IntelProbe, error) {
	root := opts.SysRoot
	if root == "" {
		root = "/"
	}

	pmuDir, err := resolveI915PMU(devicePath, root)
	if err != nil {
		return nil, err
	}

	pmuType, err := readUintFile(filepath.Join(pmuDir, "type"))
	if err != nil {
		return nil, fmt.Errorf("read PMU type: %w", err)
	}

	cpu, err := readPMUCPU(filepath.Join(pmuDir, "cpumask"))
	if err != nil {
		return nil, fmt.Errorf("read PMU cpumask: %w", err)
	}

	configs, err := readVCSEngineConfigs(pmuDir)
	if err != nil {
		return nil, err
	}

	if len(configs) == 0 {
		return nil, fmt.Errorf("no vcs*-busy events under %s", pmuDir)
	}

	fds := make([]int, 0, len(configs))
	for _, config := range configs {
		fd, openErr := syscalls.open(uint32(pmuType), config, cpu)
		if openErr != nil {
			for _, opened := range fds {
				_ = syscalls.close(opened)
			}

			return nil, fmt.Errorf("perf_event_open vcs config=0x%x: %w", config, openErr)
		}

		fds = append(fds, fd)
	}

	return &IntelProbe{
		fds:      fds,
		syscalls: syscalls,
	}, nil
}

// Sample reads the perf counters, computes a wall-time-normalized utilization
// across all vcs engines, and clamps to [0, 1]. The first call seeds the
// reference state and returns 0.
func (p *IntelProbe) Sample(ctx context.Context) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return 0, errors.New("loadprobe: Intel probe closed")
	}

	now := time.Now()

	var sum uint64

	for _, fd := range p.fds {
		val, err := p.syscalls.read(fd)
		if err != nil {
			return 0, fmt.Errorf("read perf fd: %w", err)
		}

		sum += val
	}

	if p.lastTime.IsZero() {
		p.lastVal = sum
		p.lastTime = now

		return 0, nil
	}

	// Guard against counter underflow when the kernel resets the PMU (e.g.
	// after a GPU reset / runtime PM cycle): treat a non-monotonic counter
	// as a fresh interval with zero busy time rather than wrapping uint64.
	var deltaNs uint64
	if sum >= p.lastVal {
		deltaNs = sum - p.lastVal
	}

	wallNs := now.Sub(p.lastTime).Nanoseconds()
	p.lastVal = sum
	p.lastTime = now

	if wallNs <= 0 {
		return 0, nil
	}

	utilization := float64(deltaNs) / float64(wallNs)
	if utilization > 1 {
		utilization = 1
	}

	return utilization, nil
}

// Close releases all perf event fds. Safe to call multiple times.
func (p *IntelProbe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}

	p.closed = true

	for _, fd := range p.fds {
		_ = p.syscalls.close(fd)
	}

	p.fds = nil

	return nil
}

// resolveI915PMU resolves devicePath to the PMU directory backing the
// matching i915 device. It tries the BDF-suffixed PMU name first
// (e.g. "i915_0000_03_00.0", with PCI ":" separators replaced by "_") and
// falls back to the legacy bare "i915" PMU when the kernel exposes a single
// device under the back-compat name.
func resolveI915PMU(devicePath, root string) (string, error) {
	nodeName := filepath.Base(devicePath)
	if nodeName == "" || nodeName == "." || nodeName == "/" {
		return "", fmt.Errorf("loadprobe: invalid device path %q", devicePath)
	}

	deviceLink := filepath.Join(root, "sys/class/drm", nodeName, "device")

	target, err := os.Readlink(deviceLink)
	if err != nil {
		return "", fmt.Errorf("readlink %s: %w", deviceLink, err)
	}

	bdf := filepath.Base(target)

	pmuName := "i915_" + strings.ReplaceAll(bdf, ":", "_")

	candidate := filepath.Join(root, "sys/bus/event_source/devices", pmuName)
	switch _, statErr := os.Stat(candidate); {
	case statErr == nil:
		return candidate, nil
	case !errors.Is(statErr, fs.ErrNotExist):
		return "", fmt.Errorf("stat %s: %w", candidate, statErr)
	}

	bare := filepath.Join(root, "sys/bus/event_source/devices", "i915")
	switch _, statErr := os.Stat(bare); {
	case statErr == nil:
		return bare, nil
	case !errors.Is(statErr, fs.ErrNotExist):
		return "", fmt.Errorf("stat %s: %w", bare, statErr)
	}

	return "", fmt.Errorf("loadprobe: no i915 PMU found for device %s (tried %s and i915)", bdf, pmuName)
}

var (
	vcsEventRE = regexp.MustCompile(`^vcs\d+-busy$`)
	configRE   = regexp.MustCompile(`config=0x([0-9a-fA-F]+)`)
)

// readVCSEngineConfigs returns the per-engine perf_event_attr.config values
// for every vcs*-busy event exposed by the PMU, sorted by event name so the
// summation order is stable.
func readVCSEngineConfigs(pmuDir string) ([]uint64, error) {
	eventsDir := filepath.Join(pmuDir, "events")

	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		return nil, fmt.Errorf("read events dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && vcsEventRE.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}

	slices.Sort(names)

	configs := make([]uint64, 0, len(names))
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(eventsDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		match := configRE.FindSubmatch(content)
		if match == nil {
			return nil, fmt.Errorf("event %s: malformed spec %q", name, strings.TrimSpace(string(content)))
		}

		v, parseErr := strconv.ParseUint(string(match[1]), 16, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("event %s: parse config: %w", name, parseErr)
		}

		configs = append(configs, v)
	}

	return configs, nil
}

func readUintFile(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// readPMUCPU returns the first CPU from the PMU's cpumask file. The kernel
// reports a single monitoring CPU for uncore-style PMUs as a comma-separated
// list of integers/ranges (e.g. "0", "0,4-7"). We pick the first integer.
// Falls back to CPU 0 when the file is missing.
func readPMUCPU(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}

		return 0, err
	}

	content := strings.TrimSpace(string(b))
	if content == "" {
		return 0, nil
	}

	first := content
	if idx := strings.IndexAny(first, ",-"); idx >= 0 {
		first = first[:idx]
	}

	cpu, err := strconv.Atoi(first)
	if err != nil {
		return 0, fmt.Errorf("parse cpumask %q: %w", content, err)
	}

	return cpu, nil
}
