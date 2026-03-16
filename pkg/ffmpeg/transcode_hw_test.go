//go:build hwtest

package ffmpeg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

// TestDetectHardwareEncoders_HardwarePresent verifies that DetectHardwareEncoders
// returns a non-empty list for at least one supported codec when hardware is
// available. This test only runs when the hwtest build tag is set (i.e. when
// make test detects hardware via ffmpeg -encoders). Detecting no hardware with
// the hwtest tag active is a likely bug — the test fails rather than skips.
func TestDetectHardwareEncoders_HardwarePresent(t *testing.T) {
	var foundHW bool
	for _, codec := range []ffmpeg.Codec{ffmpeg.CodecH264, ffmpeg.CodecH265} {
		if accs := ffmpeg.DetectHardwareEncoders(codec); len(accs) > 0 {
			foundHW = true
			break
		}
	}
	assert.True(t, foundHW, "DetectHardwareEncoders must return a non-empty list for at least one codec when hardware is present")
}
