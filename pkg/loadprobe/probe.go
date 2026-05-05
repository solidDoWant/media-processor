// Package loadprobe samples worker load metrics — Intel i915 GPU video-engine
// utilization or container CPU usage — and exposes a smoothed reading plus a
// fallback signal to callers gating admission decisions on observed load.
//
// The package supports Linux only. Non-Linux builds return
// ErrUnsupportedPlatform from probe constructors so callers can surface a
// fallback signal at startup without special-casing the build target.
//
// Probe values must lie in the closed interval [0, 1]; the Sampler clamps
// values slightly outside this range rather than rejecting them, so a
// transient negative value (e.g. clock skew on a perf delta) does not knock
// the sampler into fallback mode.
package loadprobe

import (
	"context"
	"errors"
)

// ErrUnsupportedPlatform is returned by probe constructors when the target
// platform cannot be probed (non-Linux build, missing kernel feature, etc.).
var ErrUnsupportedPlatform = errors.New("loadprobe: platform unsupported")

// ErrCgroupUnconstrained is returned by NewCgroupProbe when the cgroup CPU
// quota is unset (cpu.max reads "max ..."). The supplier consuming this
// package is expected to fall back to its static cap when this error is
// surfaced.
var ErrCgroupUnconstrained = errors.New("loadprobe: cgroup cpu quota unconstrained")

// Probe samples a single load metric. Each call computes a fresh utilization
// value relative to the elapsed wall time since the prior call.
//
// Implementations that read counters and compute deltas must initialize their
// reference state on the first Sample so that the first returned value is
// meaningful (the convention in this package is to return 0 on the first
// sample and an actual delta-based reading from the second onward).
type Probe interface {
	// Sample returns the instantaneous load value, ideally in the closed
	// interval [0, 1]. Callers should clamp out-of-range readings.
	Sample(ctx context.Context) (float64, error)

	// Close releases any kernel resources held by the probe. After Close
	// returns, Sample must not be called.
	Close() error
}
