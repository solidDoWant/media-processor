//go:build hwtest

package ffmpeg_test

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

// TestGetHardwareEncoder_HardwarePresent verifies that GetHardwareEncoder
// returns a non-HWAccelNone result for at least one supported codec when
// hardware is available. This test only runs when the hwtest build tag is set
// (i.e. when make test detects hardware via ffmpeg -encoders). Detecting no
// hardware with the hwtest tag active is a likely bug — the test fails rather
// than skips.
func TestGetHardwareEncoder_HardwarePresent(t *testing.T) {
	var foundHW bool

	for _, codec := range []ffmpeg.Codec{astiav.CodecIDH264, ffmpeg.CodecH265} {
		if ffmpeg.GetHardwareEncoder(codec, ffmpeg.HWAccelAuto) != ffmpeg.HWAccelNone {
			foundHW = true
			break
		}
	}

	assert.True(t, foundHW, "GetHardwareEncoder must return a hardware accelerator for at least one codec when hardware is present")
}
