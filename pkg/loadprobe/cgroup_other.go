//go:build !linux

package loadprobe

import (
	"context"
	"fmt"
)

// CgroupOptions configures the cgroup v2 CPU probe. The Linux build accepts
// CgroupRoot for test injection; the non-Linux stub ignores it.
type CgroupOptions struct {
	CgroupRoot string
}

// CgroupProbe is a non-functional placeholder so callers compile on
// non-Linux platforms.
type CgroupProbe struct{}

// NewCgroupProbe always returns ErrUnsupportedPlatform on non-Linux builds.
func NewCgroupProbe(_ CgroupOptions) (*CgroupProbe, error) {
	return nil, fmt.Errorf("cgroup cpu probe: %w", ErrUnsupportedPlatform)
}

// Sample is unreachable on non-Linux builds since NewCgroupProbe never
// returns a non-nil probe; the method exists so the interface is satisfied.
func (p *CgroupProbe) Sample(_ context.Context) (float64, error) {
	return 0, ErrUnsupportedPlatform
}

// Close is a no-op on non-Linux builds.
func (p *CgroupProbe) Close() error { return nil }
