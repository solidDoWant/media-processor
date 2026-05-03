package ffmpeg_test

import (
	"context"
	"errors"
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
	progressCh := make(chan ffmpeg.Progress, 4096)

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
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

	// After progress has reached a substantial fraction of the file, no
	// subsequent update should report 0% — that indicates the sender read
	// pts from a packet whose data had already been consumed (e.g. by
	// av_interleaved_write_frame, which takes ownership of the packet).
	maxSeen := 0.0
	for _, update := range updates {
		if update.PercentComplete > maxSeen {
			maxSeen = update.PercentComplete
		}

		if maxSeen >= 50 {
			assert.NotEqual(t, float64(0), update.PercentComplete,
				"progress dropped back to 0%% after reaching %.2f%% (frames=%d) — likely reading pts from a consumed packet",
				maxSeen, update.FramesProcessed)
		}
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
	var (
		ac3Stream          *ffprobe.StreamInfo
		regularAudioStream *ffprobe.StreamInfo
	)

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

// attachmentInfo describes an attachment stream found in a media file.
type attachmentInfo struct {
	data     []byte
	mimeType string
	filename string
}

// probeAttachments opens path and returns attachment stream info.
// The matroska demuxer reads MKV attachment elements as video streams with
// AV_DISPOSITION_ATTACHED_PIC; they are identified by a "mimetype" metadata
// key. Image data is returned from the first packet on each such stream.
func probeAttachments(t *testing.T, path string) []attachmentInfo {
	t.Helper()

	fmtCtx := astiav.AllocFormatContext()

	require.NotNil(t, fmtCtx)

	defer fmtCtx.Free()

	require.NoError(t, fmtCtx.OpenInput(path, nil, nil))

	defer fmtCtx.CloseInput()

	require.NoError(t, fmtCtx.FindStreamInfo(nil))

	// Build a map of stream index → attachmentInfo for streams that carry a
	// "mimetype" metadata tag (the signature of an MKV attachment stream).
	attachStreams := make(map[int]*attachmentInfo)

	for _, s := range fmtCtx.Streams() {
		meta := s.Metadata()
		if meta == nil {
			continue
		}

		mt := meta.Get("mimetype", nil, astiav.NewDictionaryFlags())
		if mt == nil {
			continue
		}

		info := &attachmentInfo{mimeType: mt.Value()}

		if fn := meta.Get("filename", nil, astiav.NewDictionaryFlags()); fn != nil {
			info.filename = fn.Value()
		}

		attachStreams[s.Index()] = info
	}

	// Read packets to collect image bytes for each attachment stream.
	pkt := astiav.AllocPacket()

	require.NotNil(t, pkt)

	defer pkt.Free()

	for {
		if err := fmtCtx.ReadFrame(pkt); err != nil {
			break
		}

		if info, ok := attachStreams[pkt.StreamIndex()]; ok && info.data == nil {
			d := pkt.Data()
			copied := make([]byte, len(d))
			copy(copied, d)
			info.data = copied
		}

		pkt.Unref()
	}

	result := make([]attachmentInfo, 0, len(attachStreams))
	for _, info := range attachStreams {
		result = append(result, *info)
	}

	return result
}

// TestTranscode_WithCoverArt_JPEG verifies that a JPEG cover art attachment is
// embedded in the output MKV with the correct bytes and metadata.
func TestTranscode_WithCoverArt_JPEG(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01}
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToContainer(ffmpeg.ContainerMKV).
		WithCoverArt(jpegBytes, "image/jpeg").
		Build().
		Run(t.Context())
	require.NoError(t, err)

	attachments := probeAttachments(t, output)
	require.Len(t, attachments, 1, "output must contain exactly one attachment stream")
	assert.Equal(t, jpegBytes, attachments[0].data)
	assert.Equal(t, "image/jpeg", attachments[0].mimeType)
	assert.Equal(t, "cover.jpg", attachments[0].filename)
}

// TestTranscode_WithCoverArt_PNG verifies that a PNG cover art attachment is
// embedded in the output MKV with the correct bytes and metadata.
func TestTranscode_WithCoverArt_PNG(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToContainer(ffmpeg.ContainerMKV).
		WithCoverArt(pngBytes, "image/png").
		Build().
		Run(t.Context())
	require.NoError(t, err)

	attachments := probeAttachments(t, output)
	require.Len(t, attachments, 1, "output must contain exactly one attachment stream")
	assert.Equal(t, pngBytes, attachments[0].data)
	assert.Equal(t, "image/png", attachments[0].mimeType)
	assert.Equal(t, "cover.png", attachments[0].filename)
}

// TestTranscode_WithCoverArt_ExistingAttachmentsStripped verifies that when
// cover art is provided, any existing attachment streams in the source file are
// excluded from the output — only the new poster is embedded.
func TestTranscode_WithCoverArt_ExistingAttachmentsStripped(t *testing.T) {
	oldArt := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01}
	newArt := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

	dir := t.TempDir()

	// Step 1: create an MKV with an existing JPEG attachment.
	withOld := filepath.Join(dir, "with_old.mkv")
	err := ffmpeg.NewTranscode(testVideoPath, withOld).
		ToContainer(ffmpeg.ContainerMKV).
		WithCoverArt(oldArt, "image/jpeg").
		Build().
		Run(t.Context())
	require.NoError(t, err)

	// Sanity check: the intermediate file has exactly one attachment.
	require.Len(t, probeAttachments(t, withOld), 1, "intermediate file must have one attachment")

	// Step 2: transcode the MKV again with new PNG artwork. The old attachment
	// must be stripped and only the new one embedded.
	withNew := filepath.Join(dir, "with_new.mkv")
	err = ffmpeg.NewTranscode(withOld, withNew).
		ToContainer(ffmpeg.ContainerMKV).
		WithCoverArt(newArt, "image/png").
		Build().
		Run(t.Context())
	require.NoError(t, err)

	attachments := probeAttachments(t, withNew)
	require.Len(t, attachments, 1, "output must contain exactly one attachment (old one stripped)")
	assert.Equal(t, newArt, attachments[0].data)
	assert.Equal(t, "image/png", attachments[0].mimeType)
}

// TestTranscode_WithCoverArt_NilBytesIsNoop verifies that calling WithCoverArt
// with nil bytes is a no-op: the output contains no attachment streams.
func TestTranscode_WithCoverArt_NilBytesIsNoop(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToContainer(ffmpeg.ContainerMKV).
		WithCoverArt(nil, "image/jpeg").
		Build().
		Run(t.Context())
	require.NoError(t, err)

	assert.Empty(t, probeAttachments(t, output), "no attachment expected when cover art bytes are nil")
}

// testBlackBarsVideoPath is a 320x220 H.264 video with 22-pixel black bars on the
// top and bottom, producing a 320x176 active picture area (crop=320:176:0:22).
const testBlackBarsVideoPath = "testdata/video_black_bars.mp4"

// TestWithCrop_NarrowsOutputDimensions verifies that WithCrop reduces the output
// video dimensions to the crop region (320x176 from the 320x220 source).
func TestWithCrop_NarrowsOutputDimensions(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	crop := &ffmpeg.CropParams{W: 320, H: 176, X: 0, Y: 22}

	err := ffmpeg.NewTranscode(testBlackBarsVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
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

	require.NotNil(t, videoStream, "output file should contain a video stream")
	assert.Equal(t, 320, videoStream.WidthPixels, "output width should match crop width")
	assert.Equal(t, 176, videoStream.HeightPixels, "output height should match crop height")
}

// TestWithoutCrop_DimensionsUnchanged verifies that omitting WithCrop preserves
// the source video dimensions (320x220).
func TestWithoutCrop_DimensionsUnchanged(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testBlackBarsVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
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

	require.NotNil(t, videoStream, "output file should contain a video stream")
	assert.Equal(t, 320, videoStream.WidthPixels, "width should be unchanged without crop")
	assert.Equal(t, 220, videoStream.HeightPixels, "height should be unchanged without crop")
}

// TestTranscode_H265_VideoTimestampsValid verifies that transcoding to H.265
// produces output with correct video timestamps: every video packet must have a
// valid PTS (not AV_NOPTS_VALUE), packet PTS values must not be duplicated,
// and the output frame rate must match the input.
//
// This test guards against a regression where enabling multi-threaded decoding
// caused the H.264 decoder to emit frames with AV_NOPTS_VALUE after the first
// few frames, producing output with corrupted timestamps.
func TestTranscode_H265_VideoTimestampsValid(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		Build().
		Run(t.Context())
	require.NoError(t, err)

	// Verify frame rate is preserved in the output container metadata.
	outputInfo, err := ffprobe.Probe(t.Context(), output)
	require.NoError(t, err)

	var videoStream *ffprobe.StreamInfo

	for i, stream := range outputInfo.Streams {
		if stream.CodecType == ffprobe.CodecTypeVideo {
			videoStream = &outputInfo.Streams[i]
			break
		}
	}

	require.NotNil(t, videoStream, "output must contain a video stream")

	inputInfo, err := ffprobe.Probe(t.Context(), testVideoPath)
	require.NoError(t, err)

	var inputVideoStream *ffprobe.StreamInfo

	for i, stream := range inputInfo.Streams {
		if stream.CodecType == ffprobe.CodecTypeVideo {
			inputVideoStream = &inputInfo.Streams[i]
			break
		}
	}

	require.NotNil(t, inputVideoStream)
	assert.InDelta(t, inputVideoStream.FramesPerSecond, videoStream.FramesPerSecond, 0.5,
		"output frame rate must match input")

	// Open the output file with go-astiav and verify all video packet PTS values.
	fmtCtx := astiav.AllocFormatContext()
	require.NotNil(t, fmtCtx)

	defer fmtCtx.Free()

	require.NoError(t, fmtCtx.OpenInput(output, nil, nil))
	defer fmtCtx.CloseInput()

	require.NoError(t, fmtCtx.FindStreamInfo(nil))

	videoStreamIndex := -1

	for _, stream := range fmtCtx.Streams() {
		if stream.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			videoStreamIndex = stream.Index()
			break
		}
	}

	require.NotEqual(t, -1, videoStreamIndex)

	pkt := astiav.AllocPacket()
	require.NotNil(t, pkt)

	defer pkt.Free()

	var packetCount int

	// Collect PTS values to verify they form a valid, monotonic display order.
	var ptsValues []int64

	for {
		if err := fmtCtx.ReadFrame(pkt); err != nil {
			require.True(t, errors.Is(err, astiav.ErrEof), "unexpected ReadFrame error: %v", err)

			break
		}

		if pkt.StreamIndex() != videoStreamIndex {
			pkt.Unref()
			continue
		}

		packetCount++
		pts := pkt.Pts()
		require.NotEqual(t, astiav.NoPtsValue, pts,
			"video packet %d must have a valid PTS", packetCount)
		ptsValues = append(ptsValues, pts)

		pkt.Unref()
	}

	assert.Greater(t, packetCount, 0, "output must contain video packets")

	// Verify that no two video packets share a PTS value.
	seen := make(map[int64]bool, len(ptsValues))

	for i, pts := range ptsValues {
		assert.False(t, seen[pts],
			"duplicate PTS value %d at packet %d", pts, i)
		seen[pts] = true
	}
}

// TestGetHardwareEncoder_ValidResult verifies that GetHardwareEncoder returns
// only valid HWAccel constants for each supported codec. The test is
// self-adapting: it passes whether or not hardware is present (HWAccelNone is
// also a valid result).
func TestGetHardwareEncoder_ValidResult(t *testing.T) {
	validValues := []ffmpeg.HWAccel{
		ffmpeg.HWAccelNone,
		ffmpeg.HWAccelNVENC,
		ffmpeg.HWAccelVAAPI,
		ffmpeg.HWAccelQSV,
	}

	for _, codec := range []ffmpeg.Codec{astiav.CodecIDH264, ffmpeg.CodecH265} {
		hw := ffmpeg.GetHardwareEncoder(codec, ffmpeg.HWAccelAuto)
		assert.Contains(t, validValues, hw, "GetHardwareEncoder(%v, HWAccelAuto) returned invalid HWAccel %v", codec, hw)
	}
}
