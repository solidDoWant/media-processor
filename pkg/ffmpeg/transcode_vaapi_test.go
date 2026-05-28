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

// TestTranscode_SoftwareDecodeToVAAPI verifies that a source whose codec has no
// Intel hardware decoder (mpeg4-ASP in AVI) transcodes successfully to H.265
// with the VAAPI encoder when no crop is applied. This exercises the software
// decode + hardware encode path: each decoded frame is converted on the CPU
// (yuv420p→NV12) and then uploaded to a GPU surface before encoding.
//
// A regression allocated the CPU scaler destination and the GPU upload surface
// into the same frame field, so the software scaler wrote into a hardware
// surface and ffmpeg aborted with "scaling video frame: Invalid argument"
// (swscale's "bad dst image pointers"). Requires VAAPI hardware (vaapitest
// build tag).
func TestTranscode_SoftwareDecodeToVAAPI(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testMpeg4AVISourcePath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelVAAPI).
		Build().
		Run(t.Context())
	require.NoError(t, err, "VAAPI transcode of a software-decoded source must succeed")

	info, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)

	var foundH265 bool

	for _, stream := range info.Streams {
		if stream.CodecType == ffprobe.CodecTypeVideo && stream.CodecName == "hevc" {
			foundH265 = true
			break
		}
	}

	assert.True(t, foundH265, "output must contain an H.265 video stream")
}
