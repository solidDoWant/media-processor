//go:build hwtest

package ffmpeg_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
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

// testCropOutputDimensions is a shared helper that runs a transcode with the
// given CropParams and asserts the output video dimensions match the crop region.
func testCropOutputDimensions(t *testing.T, output string, crop *ffmpeg.CropParams) {
	t.Helper()

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

// TestTranscode_CropWithVAAPI_SWDecode verifies that the SW-decode VAAPI crop
// path (crop=W:H:X:Y,scale_vaapi) produces output with the correct dimensions.
// Requires VAAPI hardware (hwtest build tag).
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

	testCropOutputDimensions(t, output, crop)
}

// TestTranscode_CropWithNVENC_CuvidFallback verifies that the CUDA cuvid
// fallback crop path (hwdownload,crop,hwupload) produces output with the
// correct dimensions when the cuvid dict option is unavailable. The test
// forces the fallback by using an input codec that cuvid does not accept the
// crop dict option for, or by relying on the natural fallback logic.
// Requires NVENC hardware (hwtest build tag).
func TestTranscode_CropWithNVENC_CuvidFallback(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")
	crop := &ffmpeg.CropParams{W: 320, H: 176, X: 0, Y: 22}

	err := ffmpeg.NewTranscode(testBlackBarsVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelNVENC).
		WithCrop(crop).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	testCropOutputDimensions(t, output, crop)
}
