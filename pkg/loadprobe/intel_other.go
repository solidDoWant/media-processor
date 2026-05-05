//go:build !linux

package loadprobe

import (
	"context"
	"fmt"
)

// IntelOptions configures the i915 PMU probe. The Linux build accepts
// SysRoot for test injection; the non-Linux stub ignores it.
type IntelOptions struct {
	SysRoot string
}

// IntelProbe is a non-functional placeholder so callers compile on non-Linux
// platforms. NewIntelProbe always returns ErrUnsupportedPlatform.
type IntelProbe struct{}

// NewIntelProbe always returns ErrUnsupportedPlatform on non-Linux builds.
func NewIntelProbe(_ string, _ IntelOptions) (*IntelProbe, error) {
	return nil, fmt.Errorf("intel i915 probe: %w", ErrUnsupportedPlatform)
}

// Sample is unreachable on non-Linux builds since NewIntelProbe never returns
// a non-nil probe; the method exists so the interface is satisfied.
func (p *IntelProbe) Sample(_ context.Context) (float64, error) {
	return 0, ErrUnsupportedPlatform
}

// Close is a no-op on non-Linux builds.
func (p *IntelProbe) Close() error { return nil }
