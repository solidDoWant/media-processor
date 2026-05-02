//go:build qsvtest

package ffmpeg_test

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

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
		ToAudioCodec(ffmpeg.CodecCopy).
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
