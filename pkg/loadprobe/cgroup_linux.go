//go:build linux

package loadprobe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CgroupOptions configures the cgroup v2 CPU probe.
type CgroupOptions struct {
	// CgroupRoot is the directory holding cpu.max and cpu.stat. Defaults to
	// "/sys/fs/cgroup" (the unified cgroup v2 hierarchy mounted at its
	// canonical path inside containers).
	CgroupRoot string
}

// CgroupProbe samples container CPU utilization from cgroup v2 cpu.stat. It
// returns the rolling cpu.usage_usec delta divided by the allowed CPU
// bandwidth (quota_us × Δwall_us / period_us) — i.e. 1.0 when the workload
// is consuming its full quota.
type CgroupProbe struct {
	statPath string
	quotaUs  uint64
	periodUs uint64
	now      func() time.Time

	mu       sync.Mutex
	lastUsg  uint64
	lastTime time.Time
	closed   bool
}

// NewCgroupProbe binds a probe to the cgroup v2 hierarchy under
// opts.CgroupRoot. Returns ErrCgroupUnconstrained when cpu.max reports
// "max" (no quota set), and a wrapped filesystem error when cpu.max or
// cpu.stat are missing — both treated as init failures by callers.
func NewCgroupProbe(opts CgroupOptions) (*CgroupProbe, error) {
	return newCgroupProbe(opts, time.Now)
}

func newCgroupProbe(opts CgroupOptions, now func() time.Time) (*CgroupProbe, error) {
	root := opts.CgroupRoot
	if root == "" {
		root = "/sys/fs/cgroup"
	}

	quotaUs, periodUs, err := readCPUMax(filepath.Join(root, "cpu.max"))
	if err != nil {
		return nil, err
	}

	statPath := filepath.Join(root, "cpu.stat")
	if _, err := os.Stat(statPath); err != nil {
		return nil, fmt.Errorf("stat %s: %w", statPath, err)
	}

	return &CgroupProbe{
		statPath: statPath,
		quotaUs:  quotaUs,
		periodUs: periodUs,
		now:      now,
	}, nil
}

// Sample reads cpu.stat and returns the wall-time-normalized CPU utilization
// since the previous Sample. The first call seeds the reference state and
// returns 0.
func (p *CgroupProbe) Sample(ctx context.Context) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return 0, errors.New("loadprobe: cgroup probe closed")
	}

	usage, err := readUsageUsec(p.statPath)
	if err != nil {
		return 0, err
	}

	now := p.now()

	if p.lastTime.IsZero() {
		p.lastUsg = usage
		p.lastTime = now

		return 0, nil
	}

	// Guard against counter regression — cgroup v2 cpu.stat usage_usec is
	// monotonic in normal operation, but treat any unexpected non-monotonic
	// reading as a zero-busy interval rather than letting uint64 subtraction
	// wrap.
	var deltaUs uint64
	if usage >= p.lastUsg {
		deltaUs = usage - p.lastUsg
	}

	wallUs := now.Sub(p.lastTime).Microseconds()
	p.lastUsg = usage
	p.lastTime = now

	if wallUs <= 0 || p.quotaUs == 0 {
		return 0, nil
	}

	// utilization = Δusage_us × period_us / (quota_us × Δwall_us)
	utilization := (float64(deltaUs) * float64(p.periodUs)) / (float64(p.quotaUs) * float64(wallUs))
	if utilization > 1 {
		utilization = 1
	}

	return utilization, nil
}

// Close marks the probe closed. The cgroup probe holds no kernel resources;
// Close exists to satisfy the Probe interface.
func (p *CgroupProbe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	return nil
}

func readCPUMax(path string) (quota, period uint64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}

	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected cpu.max format %q", strings.TrimSpace(string(b)))
	}

	if fields[0] == "max" {
		return 0, 0, ErrCgroupUnconstrained
	}

	quota, err = strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse cpu.max quota %q: %w", fields[0], err)
	}

	period, err = strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse cpu.max period %q: %w", fields[1], err)
	}

	if period == 0 {
		return 0, 0, fmt.Errorf("cpu.max period is 0")
	}

	return quota, period, nil
}

func readUsageUsec(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		rest, ok := strings.CutPrefix(line, "usage_usec ")
		if !ok {
			continue
		}

		return strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan %s: %w", path, err)
	}

	return 0, fmt.Errorf("usage_usec not found in %s", path)
}
