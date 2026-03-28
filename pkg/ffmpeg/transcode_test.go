package ffmpeg_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// probeStreamDispositions opens a media file and returns a map of stream index
// to DispositionFlags by reading stream metadata via go-astiav directly.
// This is used in tests because ffprobe.StreamInfo does not expose disposition.
func probeStreamDispositions(t *testing.T, path string) map[int]astiav.DispositionFlags {
	t.Helper()
	fmtCtx := astiav.AllocFormatContext()
	require.NotNil(t, fmtCtx, "failed to allocate format context")
	defer fmtCtx.Free()
	require.NoError(t, fmtCtx.OpenInput(path, nil, nil))
	defer fmtCtx.CloseInput()
	require.NoError(t, fmtCtx.FindStreamInfo(nil))
	result := make(map[int]astiav.DispositionFlags)
	for _, s := range fmtCtx.Streams() {
		result[s.Index()] = s.DispositionFlags()
	}
	return result
}

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

// TestWithHardwareDevice_EmptyString verifies that passing an empty string to
// WithHardwareDevice is a no-op: the transcode completes successfully and
// produces valid MKV output.
func TestWithHardwareDevice_EmptyString(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToAudioCodec(ffmpeg.CodecCopy).
		ToContainer(ffmpeg.ContainerMKV).
		WithHardwareDevice("").
		Build().
		Run(t.Context())
	require.NoError(t, err)

	_, statErr := os.Stat(output)
	require.NoError(t, statErr, "output file must exist")

	info, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)
	assert.Equal(t, "matroska,webm", info.Format)
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
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after context cancellation")
	}
}

// TestTranscode_ExcludeStreams verifies that a stream index passed to
// ExcludeStreams is absent from the output file.
func TestTranscode_ExcludeStreams(t *testing.T) {
	inputInfo, err := ffprobe.Probe(t.Context(), testVideoPath)
	require.NoError(t, err)

	// Locate the first audio stream index to exclude.
	audioIndex := -1
	for _, s := range inputInfo.Streams {
		if s.CodecType == ffprobe.CodecTypeAudio {
			audioIndex = s.Index
			break
		}
	}
	require.NotEqual(t, -1, audioIndex, "test fixture must have at least one audio stream")

	output := filepath.Join(t.TempDir(), "out.mkv")
	err = ffmpeg.NewTranscode(testVideoPath, output).
		ToContainer(ffmpeg.ContainerMKV).
		ExcludeStreams(audioIndex).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	outputInfo, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)

	for _, s := range outputInfo.Streams {
		assert.NotEqual(t, ffprobe.CodecTypeAudio, s.CodecType,
			"excluded audio stream must not appear in the output")
	}
	assert.Equal(t, len(inputInfo.Streams)-1, len(outputInfo.Streams),
		"output must have one fewer stream than the input")
}

// TestTranscode_DispositionPreserved verifies that input stream dispositions
// are copied to output streams unchanged when no WithDefault* setter is called.
func TestTranscode_DispositionPreserved(t *testing.T) {
	inputDisps := probeStreamDispositions(t, testVideoPath)
	output := filepath.Join(t.TempDir(), "out.mkv")
	require.NoError(t, ffmpeg.NewTranscode(testVideoPath, output).
		ToContainer(ffmpeg.ContainerMKV).
		Build().
		Run(t.Context()))

	outputDisps := probeStreamDispositions(t, output)
	require.Len(t, outputDisps, len(inputDisps), "stream count must match")
	for idx, inputDisp := range inputDisps {
		assert.Equal(t, inputDisp, outputDisps[idx],
			"stream %d: disposition must be preserved from input", idx)
	}
}

// TestTranscode_WithDefaultAudioStream verifies that WithDefaultAudioStream
// marks the specified audio stream as default and clears the flag from all
// other audio streams.
func TestTranscode_WithDefaultAudioStream(t *testing.T) {
	// Test fixture (video.mp4): stream 0 = video, stream 1 = audio (both default=1 in input).
	tests := []struct {
		name        string
		audioIdx    int
		wantDefault bool
	}{
		{
			name:        "specified audio stream index is marked default",
			audioIdx:    1, // audio stream index in test fixture
			wantDefault: true,
		},
		{
			name:        "audio stream loses default when a different index is designated",
			audioIdx:    999, // nonexistent; stream 1 is the only audio and loses its default
			wantDefault: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "out.mkv")
			require.NoError(t, ffmpeg.NewTranscode(testVideoPath, output).
				ToContainer(ffmpeg.ContainerMKV).
				WithDefaultAudioStream(&tt.audioIdx).
				Build().
				Run(t.Context()))

			outInfo, err := ffprobe.Probe(t.Context(), output)
			require.NoError(t, err)
			disps := probeStreamDispositions(t, output)

			for _, s := range outInfo.Streams {
				if s.CodecType == ffprobe.CodecTypeAudio {
					assert.Equal(t, tt.wantDefault,
						disps[s.Index].Has(astiav.DispositionFlagDefault),
						"audio stream %d: default disposition mismatch", s.Index)
				}
			}
		})
	}
}

// TestTranscode_WithDownmix verifies that WithDownmix appends an additional
// AC-3 encoded audio stream derived from the nominated source stream, inherits
// the source stream's language tag, and only receives the default disposition
// when no other audio stream is already marked default.
func TestTranscode_WithDownmix(t *testing.T) {
	inputInfo, err := ffprobe.Probe(t.Context(), testVideoPath)
	require.NoError(t, err)

	audioIndex := -1
	for _, s := range inputInfo.Streams {
		if s.CodecType == ffprobe.CodecTypeAudio {
			audioIndex = s.Index
			break
		}
	}
	require.NotEqual(t, -1, audioIndex, "test fixture must have at least one audio stream")

	output := filepath.Join(t.TempDir(), "out.mkv")
	err = ffmpeg.NewTranscode(testVideoPath, output).
		ToContainer(ffmpeg.ContainerMKV).
		WithDownmix(&audioIndex).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	outputInfo, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)

	// Expect one more stream than the input (the downmix).
	assert.Equal(t, len(inputInfo.Streams)+1, len(outputInfo.Streams),
		"output must contain one additional stream from the downmix")

	// Locate the AC-3 downmix stream and verify its properties.
	var ac3Stream *ffprobe.StreamInfo
	var regularAudioStream *ffprobe.StreamInfo
	for i, s := range outputInfo.Streams {
		if s.CodecType != ffprobe.CodecTypeAudio {
			continue
		}
		if s.CodecName == "ac3" {
			ac3Stream = &outputInfo.Streams[i]
		} else if regularAudioStream == nil {
			regularAudioStream = &outputInfo.Streams[i]
		}
	}
	require.NotNil(t, ac3Stream, "output must contain an AC-3 audio stream from the downmix")

	// Language tag must match the regular output audio stream. Both are derived
	// from the same source, so the MKV muxer produces the same tag for both
	// (e.g. both empty when the source is "und", both "eng" when tagged "eng").
	if regularAudioStream != nil {
		assert.Equal(t, regularAudioStream.Tags["language"], ac3Stream.Tags["language"],
			"downmix stream language must match the regular output audio stream")
	}

	// Determine whether any non-downmix audio stream is already default so we
	// can assert the correct default disposition on the downmix.
	outputDisps := probeStreamDispositions(t, output)
	var existingDefault bool
	for _, s := range outputInfo.Streams {
		if s.CodecType == ffprobe.CodecTypeAudio && s.CodecName != "ac3" {
			if outputDisps[s.Index].Has(astiav.DispositionFlagDefault) {
				existingDefault = true
				break
			}
		}
	}
	// Downmix should be default only when no other audio stream is default.
	assert.Equal(t, !existingDefault,
		outputDisps[ac3Stream.Index].Has(astiav.DispositionFlagDefault),
		"downmix default disposition must be set iff no other audio stream is already default")
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
