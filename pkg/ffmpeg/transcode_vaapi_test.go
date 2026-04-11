//go:build vaapitest

package ffmpeg_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// TestTranscode_CropWithVAAPI_SWDecode verifies that the SW-decode VAAPI crop
// path (crop=W:H:X:Y,scale_vaapi) produces output with the correct dimensions.
// Requires VAAPI hardware (vaapitest build tag).
func TestTranscode_CropWithVAAPI_SWDecode(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")
	crop := &ffmpeg.CropParams{W: 320, H: 176, X: 0, Y: 22}

	err := ffmpeg.NewTranscode(testBlackBarsVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelVAAPI).
		WithCrop(crop).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	info, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)

	var videoStream *ffprobe.StreamInfo
	for i := range info.Streams {
		if info.Streams[i].CodecType == ffprobe.CodecTypeVideo {
			videoStream = &info.Streams[i]
			break
		}
	}

	require.NotNil(t, videoStream, "output must contain a video stream")
	assert.Equal(t, crop.W, videoStream.WidthPixels, "output width must match crop width")
	assert.Equal(t, crop.H, videoStream.HeightPixels, "output height must match crop height")
}
