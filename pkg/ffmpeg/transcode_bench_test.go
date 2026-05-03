package ffmpeg_test

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// benchVideoPath is the path to a 10-minute looped version of testVideoPath,
// created once in TestMain and shared by both transcode benchmarks so that
// encode time dominates over fixed costs like ffmpeg process startup.
var benchVideoPath string

// TestMain creates a 10-minute looped version of the test fixture before any
// test or benchmark in this package runs, then cleans it up afterwards.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ffmpeg-bench-*")
	if err != nil {
		log.Fatalf("create bench fixture dir: %v", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	benchVideoPath = filepath.Join(dir, "bench_10m.mp4")

	// Re-encode the unit-test clip in a 10-minute loop. Using -c copy with
	// -stream_loop produces NAL-unit corruption at each boundary because AVCC
	// size prefixes from the original avcC box do not align correctly after
	// concatenation. Re-encoding with libx264 produces a clean, single-stream
	// bitstream with no seams, and preserves the codec parameters that the
	// hardware decoder expects.
	out, err := exec.Command("ffmpeg", "-y",
		"-stream_loop", "-1",
		"-i", testVideoPath,
		"-t", "600",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-c:a", "copy",
		benchVideoPath,
	).CombinedOutput()
	if err != nil {
		log.Fatalf("create bench fixture: %v\n%s", err, out)
	}

	os.Exit(m.Run())
}

// cliArgsForHW returns the ffmpeg CLI flags that match the hardware path
// selected by TranscodeBuilder. preInputArgs go before -i, postInputArgs go
// between -i and -c:v, and encoderName is the value passed to -c:v.
func cliArgsForHW(hw ffmpeg.HWAccel) (preInputArgs []string, encoderName string, postInputArgs []string) {
	switch hw {
	case ffmpeg.HWAccelQSV:
		return []string{"-hwaccel", "qsv"}, "hevc_qsv", nil
	case ffmpeg.HWAccelNVENC:
		return []string{"-hwaccel", "cuda"}, "hevc_nvenc", nil
	case ffmpeg.HWAccelVAAPI:
		// VAAPI requires explicit frame upload on the CLI; the library handles
		// this internally via its software scale context.
		return []string{"-vaapi_device", "/dev/dri/renderD128"}, "hevc_vaapi",
			[]string{"-vf", "format=nv12,hwupload"}
	default:
		return nil, "libx265", nil
	}
}

// BenchmarkTranscode_H265_MKV measures the throughput of TranscodeBuilder when
// encoding a 10-minute fixture to H.265/MKV. Hardware acceleration is used
// when available (HWAccelAuto), falling back to software libx265.
//
// Custom metrics frames/sec and x-realtime are reported so results can be
// compared directly against BenchmarkTranscodeFFmpegCLI_H265_MKV.
func BenchmarkTranscode_H265_MKV(b *testing.B) {
	hw := ffmpeg.GetHardwareEncoder(ffmpeg.CodecH265, ffmpeg.HWAccelAuto)
	b.Logf("hardware accelerator: %v", hw)

	info, err := ffprobe.Probe(b.Context(), benchVideoPath)
	require.NoError(b, err)

	inputDuration := info.Duration.Seconds()

	var inputFPS float64

	for _, stream := range info.Streams {
		if stream.CodecType == ffprobe.CodecTypeVideo {
			inputFPS = stream.FramesPerSecond
			break
		}
	}

	inputFrames := int64(inputDuration * inputFPS)

	b.ResetTimer()

	for range b.N {
		output := filepath.Join(b.TempDir(), "out.mkv")

		err := ffmpeg.NewTranscode(benchVideoPath, output).
			ToVideoCodec(ffmpeg.CodecH265).
			ToContainer(ffmpeg.ContainerMKV).
			HardwareAccel(hw).
			Build().
			Run(b.Context())
		require.NoError(b, err)
	}

	b.StopTimer()

	elapsed := b.Elapsed()
	b.ReportMetric(float64(inputFrames*int64(b.N))/elapsed.Seconds(), "frames/sec")
	b.ReportMetric(inputDuration*float64(b.N)/elapsed.Seconds(), "x-realtime")
}

// BenchmarkTranscodeFFmpegCLI_H265_MKV measures the throughput of a direct
// ffmpeg CLI invocation with settings equivalent to BenchmarkTranscode_H265_MKV.
// Both benchmarks call GetHardwareEncoder so they exercise the same encode path,
// making their x-realtime figures directly comparable.
func BenchmarkTranscodeFFmpegCLI_H265_MKV(b *testing.B) {
	hw := ffmpeg.GetHardwareEncoder(ffmpeg.CodecH265, ffmpeg.HWAccelAuto)
	preInputArgs, encoderName, postInputArgs := cliArgsForHW(hw)
	b.Logf("hardware accelerator: %v (encoder: %s)", hw, encoderName)

	info, err := ffprobe.Probe(b.Context(), benchVideoPath)
	require.NoError(b, err)

	inputDuration := info.Duration.Seconds()

	var inputFPS float64

	for _, stream := range info.Streams {
		if stream.CodecType == ffprobe.CodecTypeVideo {
			inputFPS = stream.FramesPerSecond
			break
		}
	}

	inputFrames := int64(inputDuration * inputFPS)

	b.ResetTimer()

	for range b.N {
		output := filepath.Join(b.TempDir(), "out.mkv")

		args := []string{"-y"}
		args = append(args, preInputArgs...)
		args = append(args, "-i", benchVideoPath)
		args = append(args, postInputArgs...)
		args = append(args, "-c:v", encoderName, "-c:a", "copy", output)

		require.NoError(b, exec.CommandContext(b.Context(), "ffmpeg", args...).Run())
	}

	b.StopTimer()

	elapsed := b.Elapsed()
	b.ReportMetric(float64(inputFrames*int64(b.N))/elapsed.Seconds(), "frames/sec")
	b.ReportMetric(inputDuration*float64(b.N)/elapsed.Seconds(), "x-realtime")
}
