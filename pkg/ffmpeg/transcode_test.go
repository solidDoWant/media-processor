package ffmpeg_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// testVideoPath is the shared input file for all transcode tests.
const testVideoPath = "../../pkg/ffprobe/testdata/video.mp4"

// TestTranscode_H265_MKV verifies that transcoding to H.265/MKV produces a
// valid output file containing an H.265 video stream.
func TestTranscode_H265_MKV(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToAudioCodec(ffmpeg.CodecCopy).
		ToContainer(ffmpeg.ContainerMKV).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	// Verify output exists and is a valid MKV with H.265 video.
	_, statErr := os.Stat(output)
	require.NoError(t, statErr, "output file must exist")

	info, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)
	assert.Equal(t, "matroska,webm", info.Format)

	var foundH265 bool
	for _, s := range info.Streams {
		if s.CodecType == ffprobe.CodecTypeVideo && s.CodecName == "hevc" {
			foundH265 = true
			break
		}
	}
	assert.True(t, foundH265, "output must contain an H.265 video stream")
}

// TestTranscode_DefaultSettings verifies that calling NewTranscode with no
// additional options (copy-all defaults) produces a valid output whose codec,
// resolution, frame rate, sample rate, channel count, and bitrate all match
// the input file.
func TestTranscode_DefaultSettings(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mp4")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	_, statErr := os.Stat(output)
	require.NoError(t, statErr, "output file must exist")

	inputInfo, err := ffprobe.Probe(t.Context(), testVideoPath)
	require.NoError(t, err)

	outputInfo, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)

	require.Equal(t, len(inputInfo.Streams), len(outputInfo.Streams), "stream count must match")

	for i, inStream := range inputInfo.Streams {
		outStream := outputInfo.Streams[i]

		assert.Equal(t, inStream.CodecName, outStream.CodecName,
			"stream %d: codec name must match", i)
		assert.Equal(t, inStream.CodecType, outStream.CodecType,
			"stream %d: codec type must match", i)
		assert.Equal(t, inStream.BitsPerSecond, outStream.BitsPerSecond,
			"stream %d: bitrate must match", i)

		if inStream.CodecType == ffprobe.CodecTypeVideo {
			assert.Equal(t, inStream.WidthPixels, outStream.WidthPixels,
				"stream %d: width must match", i)
			assert.Equal(t, inStream.HeightPixels, outStream.HeightPixels,
				"stream %d: height must match", i)
			assert.InDelta(t, inStream.FramesPerSecond, outStream.FramesPerSecond, 0.01,
				"stream %d: frame rate must match", i)
		}

		if inStream.CodecType == ffprobe.CodecTypeAudio {
			assert.Equal(t, inStream.AudioSampleRateHz, outStream.AudioSampleRateHz,
				"stream %d: audio sample rate must match", i)
			assert.Equal(t, inStream.AudioChannelCount, outStream.AudioChannelCount,
				"stream %d: audio channel count must match", i)
		}
	}
}

// TestTranscode_HWAccelAuto runs with HWAccelAuto and expects either hardware
// or software encoding to succeed without error.
func TestTranscode_HWAccelAuto(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToAudioCodec(ffmpeg.CodecCopy).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelAuto).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	// Output must be a valid MKV with H.265.
	info, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)

	var foundH265 bool
	for _, s := range info.Streams {
		if s.CodecType == ffprobe.CodecTypeVideo && s.CodecName == "hevc" {
			foundH265 = true
			break
		}
	}
	assert.True(t, foundH265, "output must contain an H.265 video stream regardless of HW path")
}

// TestTranscode_ProgressChannel verifies that at least one progress update is
// received on the provided channel.
func TestTranscode_ProgressChannel(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")
	progressCh := make(chan ffmpeg.Progress, 256)

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToAudioCodec(ffmpeg.CodecCopy).
		ToContainer(ffmpeg.ContainerMKV).
		WithProgressChan(progressCh).
		Build().
		Run(t.Context())
	require.NoError(t, err)
	close(progressCh)

	var updates []ffmpeg.Progress
	for p := range progressCh {
		updates = append(updates, p)
	}
	assert.NotEmpty(t, updates, "must receive at least one progress update")

	for _, p := range updates {
		assert.GreaterOrEqual(t, p.FramesProcessed, int64(1))
		assert.GreaterOrEqual(t, p.PercentComplete, float64(0))
		assert.LessOrEqual(t, p.PercentComplete, float64(100))
	}
}

// TestTranscode_CancelledContext verifies that a cancelled context causes Run
// to return promptly with ctx.Err().
func TestTranscode_CancelledContext(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before calling Run

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToAudioCodec(ffmpeg.CodecCopy).
		ToContainer(ffmpeg.ContainerMKV).
		Build().
		Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestTranscode_CancelDuringRun verifies that cancellation mid-transcode
// returns promptly (within a generous deadline). A WithStartHook is used to
// cancel the context at a deterministic point — after setup is complete but
// before the first packet is read — avoiding a flaky time.Sleep.
func TestTranscode_CancelDuringRun(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- ffmpeg.NewTranscode(testVideoPath, output).
			ToVideoCodec(ffmpeg.CodecH265).
			ToAudioCodec(ffmpeg.CodecCopy).
			ToContainer(ffmpeg.ContainerMKV).
			WithStartHook(cancel).
			Build().
			Run(ctx)
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-t.Context().Done():
		t.Fatal("Run did not return promptly after context cancellation")
	}
}

// TestDetectHardwareEncoders_ValidResult verifies that DetectHardwareEncoders
// returns only valid HWAccel constants for each supported codec. The test is
// self-adapting: it passes whether or not hardware is present.
func TestDetectHardwareEncoders_ValidResult(t *testing.T) {
	validValues := []ffmpeg.HWAccel{
		ffmpeg.HWAccelNVENC,
		ffmpeg.HWAccelVAAPI,
		ffmpeg.HWAccelQSV,
	}
	for _, codec := range []ffmpeg.Codec{ffmpeg.CodecH264, ffmpeg.CodecH265} {
		accs := ffmpeg.DetectHardwareEncoders(codec)
		for _, hw := range accs {
			assert.Contains(t, validValues, hw, "DetectHardwareEncoders(%v) returned invalid HWAccel %v", codec, hw)
		}
	}
}
