//go:build hwtest

package ffmpeg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

// TestDetectHardwareEncoder_HardwarePresent verifies that DetectHardwareEncoder
// returns a non-None value for at least one supported codec when hardware is
// available. This test only runs when the hwtest build tag is set (i.e. when
// make test detects hardware via ffmpeg -encoders). Detecting no hardware with
// the hwtest tag active is a likely bug — the test fails rather than skips.
func TestDetectHardwareEncoder_HardwarePresent(t *testing.T) {
	var foundHW bool
	for _, codec := range []ffmpeg.Codec{ffmpeg.CodecH264, ffmpeg.CodecH265} {
		hw, err := ffmpeg.DetectHardwareEncoder(codec)
		require.NoError(t, err)
		if hw != ffmpeg.HWAccelNone {
			foundHW = true
			break
		}
	}
	assert.True(t, foundHW, "DetectHardwareEncoder must return a non-None value for at least one codec when hardware is present")
}
