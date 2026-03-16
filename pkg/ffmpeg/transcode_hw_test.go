//go:build hwtest

package ffmpeg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

// TestDetectHardwareEncoder_HardwarePresent verifies that DetectHardwareEncoder
// returns a non-None value when hardware is available. This test only runs when
// the hwtest build tag is set (i.e. when make test detects hardware via
// ffmpeg -encoders). Detecting no hardware with the hwtest tag active is a
// likely bug — the test fails rather than skips in that case.
func TestDetectHardwareEncoder_HardwarePresent(t *testing.T) {
	hw, err := ffmpeg.DetectHardwareEncoder()
	require.NoError(t, err)
	assert.NotEqual(t, ffmpeg.HWAccelNone, hw,
		"DetectHardwareEncoder must return a non-None value when hardware is present")
}
