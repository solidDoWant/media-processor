//go:build qsvtest

package ffmpeg_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// testVFRHEVCSourcePath is a synthetic 5 s 640x360 HEVC matroska whose
// container PTS values have been rewritten to inject a variable-frame-rate
// pattern. On Intel Arc QSV the libmfx HEVC encoder computes DecodeTimeStamp
// assuming a uniform input cadence; this fixture causes it to emit packets
// with non-monotonic DTS, which the matroska muxer rejects with
// "Application provided invalid, non monotonically increasing dts to muxer".
// The transcoder's per-stream DTS clamp in receiveAndWritePackets repairs
// the encoder output so the muxer accepts every packet.
//
// See pkg/ffmpeg/testdata/README.md for the regeneration procedure.
const testVFRHEVCSourcePath = "testdata/video_vfr_hevc.mkv"

// TestTranscode_SoftwareDecodeToQSV verifies that a source whose codec has no
// Intel hardware decoder (mpeg4-ASP in AVI) transcodes successfully to H.265
// with the QSV encoder when no crop is applied. This exercises the software
// decode + hardware encode path: each decoded frame is converted on the CPU
// (yuv420p→NV12) and then uploaded to a GPU surface before encoding.
//
// A regression allocated the CPU scaler destination and the GPU upload surface
// into the same frame field, so the software scaler wrote into a hardware
// surface and ffmpeg aborted with "scaling video frame: Invalid argument"
// (swscale's "bad dst image pointers"). Requires QSV hardware (qsvtest build
// tag).
func TestTranscode_SoftwareDecodeToQSV(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testMpeg4AVISourcePath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelQSV).
		Build().
		Run(t.Context())
	require.NoError(t, err, "QSV transcode of a software-decoded source must succeed")

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

// TestTranscode_CropWithQSV verifies that the vpp_qsv crop path produces output
// with the correct dimensions when QSV hardware is available.
// Requires QSV hardware (qsvtest build tag).
func TestTranscode_CropWithQSV(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")
	crop := &ffmpeg.CropParams{W: 320, H: 176, X: 0, Y: 22}

	err := ffmpeg.NewTranscode(testBlackBarsVideoPath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelQSV).
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

// TestTranscode_SourceWithAttachedPic_QSV verifies that a source mp4 carrying
// an embedded mjpeg cover-art stream (disposition:attached_pic) does not crash
// the QSV encoder. Before the embedded-cover-art exclusion was widened to fire
// even without fresh cover art, the still image was fed into hevc_qsv, whose
// init returned MFX_ERR_UNSUPPORTED — surfaced by libavcodec as
// "Function not implemented" — and aborted the whole transcode.
// Requires QSV hardware (qsvtest build tag).
func TestTranscode_SourceWithAttachedPic_QSV(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testAttachedPicSourcePath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelQSV).
		Build().
		Run(t.Context())
	require.NoError(t, err, "QSV transcode of a source with attached_pic must succeed; pre-fix this returned %q", "Function not implemented")
}

// TestTranscode_VFRSourceWithQSV verifies that variable-frame-rate HEVC
// sources do not break the QSV transcode pipeline. On Intel Arc, hevc_qsv's
// underlying libmfx runtime emits packets with non-monotonic DTS when fed
// frames with irregular PTS spacing, which the matroska muxer rejects with
// EINVAL. The transcoder's per-stream DTS clamp must repair the encoder
// output so that the muxer accepts every packet and the resulting file has
// strictly monotonic DTS. Requires QSV hardware (qsvtest build tag).
func TestTranscode_VFRSourceWithQSV(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mkv")

	err := ffmpeg.NewTranscode(testVFRHEVCSourcePath, output).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelQSV).
		WithCrop(&ffmpeg.CropParams{W: 624, H: 360, X: 8, Y: 0}).
		Build().
		Run(t.Context())
	require.NoError(t, err, "QSV transcode of a VFR source must succeed; pre-fix the muxer rejected non-monotonic DTS from hevc_qsv")

	// Read every video packet's DTS from the output and assert strict
	// monotonicity — the property the muxer enforces and that the clamp
	// is responsible for restoring.
	fmtCtx := astiav.AllocFormatContext()
	defer fmtCtx.Free()

	require.NotNil(t, fmtCtx)

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

	require.NotEqual(t, -1, videoStreamIndex, "output must contain a video stream")

	pkt := astiav.AllocPacket()
	require.NotNil(t, pkt)

	defer pkt.Free()

	prevDts := int64(astiav.NoPtsValue)
	packetCount := 0

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
		dts := pkt.Dts()

		if dts != astiav.NoPtsValue && prevDts != astiav.NoPtsValue {
			assert.Greater(t, dts, prevDts,
				"video packet %d DTS must be strictly greater than previous (got %d after %d)",
				packetCount, dts, prevDts)
		}

		if dts != astiav.NoPtsValue {
			prevDts = dts
		}

		pkt.Unref()
	}

	assert.Greater(t, packetCount, 0, "output must contain video packets")
}

// TestTranscode_QSVPerformanceMatchesFFmpegCLI verifies that our QSV transcode
// implementation performs within 1.25× of the ffmpeg CLI with identical QSV
// parameters. This guards against accidentally falling back to software
// encoding (which would be 5–10× slower).
func TestTranscode_QSVPerformanceMatchesFFmpegCLI(t *testing.T) {
	// --- Baseline: ffmpeg CLI with QSV ---
	cliOut := filepath.Join(t.TempDir(), "cli_out.mkv")
	cliCmd := exec.CommandContext(t.Context(),
		"ffmpeg", "-y",
		"-hwaccel", "qsv",
		"-i", testVideoPath,
		"-c:v", "hevc_qsv",
		"-c:a", "copy",
		cliOut,
	)
	cliStart := time.Now()

	if err := cliCmd.Run(); err != nil {
		t.Fatalf("ffmpeg CLI with QSV failed: %v", err)
	}

	cliDuration := time.Since(cliStart)

	// Verify CLI output is a valid MKV with H.265.
	cliInfo, err := ffprobe.Probe(t.Context(), cliOut)
	require.NoError(t, err, "ffmpeg CLI output must be a valid media file")

	var cliHasH265 bool

	for _, s := range cliInfo.Streams {
		if s.CodecType == ffprobe.CodecTypeVideo && s.CodecName == "hevc" {
			cliHasH265 = true
			break
		}
	}

	require.True(t, cliHasH265, "ffmpeg CLI output must contain H.265")

	// --- Our implementation with QSV ---
	ourOut := filepath.Join(t.TempDir(), "our_out.mkv")
	ourStart := time.Now()
	err = ffmpeg.NewTranscode(testVideoPath, ourOut).
		ToVideoCodec(ffmpeg.CodecH265).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelQSV).
		Build().
		Run(t.Context())
	require.NoError(t, err, "our QSV transcode must succeed")

	ourDuration := time.Since(ourStart)

	// Verify our output is a valid MKV with H.265.
	ourInfo, err := ffprobe.Probe(t.Context(), ourOut)
	require.NoError(t, err, "our output must be a valid media file")

	var ourHasH265 bool

	for _, s := range ourInfo.Streams {
		if s.CodecType == ffprobe.CodecTypeVideo && s.CodecName == "hevc" {
			ourHasH265 = true
			break
		}
	}

	assert.True(t, ourHasH265, "our output must contain H.265")

	// Assert our implementation is within 1.25× of the CLI baseline.
	// A factor > 1.25× strongly suggests software fallback or excessive overhead.
	maxAllowed := time.Duration(float64(cliDuration) * 1.25)
	assert.LessOrEqual(t, ourDuration, maxAllowed,
		"our QSV transcode (%v) must be within 1.25× of ffmpeg CLI (%v); "+
			"a slower result suggests software fallback", ourDuration, cliDuration)

	t.Logf("ffmpeg CLI QSV duration: %v; our QSV duration: %v (ratio: %.2f×)",
		cliDuration, ourDuration, float64(ourDuration)/float64(cliDuration))
}
