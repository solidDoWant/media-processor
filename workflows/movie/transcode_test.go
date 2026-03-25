package movie

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

func TestSelectVideoCodec(t *testing.T) {
	tests := []struct {
		name           string
		videoCodecName string
		format         string
		expected       ffmpeg.Codec
	}{
		{
			name:           "H.264 in MKV container is copied without re-encoding",
			videoCodecName: ffprobe.CodecNameH264,
			format:         "matroska,webm",
			expected:       ffmpeg.CodecCopy,
		},
		{
			name:           "H.265/HEVC in MKV container is copied without re-encoding",
			videoCodecName: ffprobe.CodecNameH265,
			format:         "matroska,webm",
			expected:       ffmpeg.CodecCopy,
		},
		{
			name:           "H.264 in MP4 container is transcoded to H.265",
			videoCodecName: ffprobe.CodecNameH264,
			format:         "mov,mp4,m4a,3gp,3g2,mj2",
			expected:       ffmpeg.CodecH265,
		},
		{
			name:           "MPEG-4 video in MKV container is transcoded to H.265",
			videoCodecName: "mpeg4",
			format:         "matroska,webm",
			expected:       ffmpeg.CodecH265,
		},
		{
			name:           "unknown codec in MKV container is transcoded to H.265",
			videoCodecName: "av1",
			format:         "matroska,webm",
			expected:       ffmpeg.CodecH265,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectVideoCodec(tt.videoCodecName, tt.format)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestRunTranscode(t *testing.T) {
	tests := []struct {
		name      string
		setupPath func(t *testing.T) string
		probe     probeOutput
		errFunc   require.ErrorAssertionFunc
		check     func(t *testing.T, outputDir string, inputPath string)
	}{
		{
			name:      "valid H.264 MP4 transcodes to output dir",
			setupPath: copyTestVideo,
			probe: probeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
			},
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir string, inputPath string) {
				baseName := filepath.Base(inputPath)
				finalPath := filepath.Join(outputDir, baseName)
				_, err := os.Stat(finalPath)
				require.NoError(t, err, "final output file should exist")

				// temp file must be cleaned up
				tempPath := filepath.Join(outputDir, "._"+baseName+".tmp")
				_, statErr := os.Stat(tempPath)
				assert.True(t, os.IsNotExist(statErr), "temp file should be removed after successful transcode")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputPath := tt.setupPath(t)
			outputDir := t.TempDir()
			input := MovieInput{FilePath: inputPath}

			err := runTranscode(t.Context(), input, tt.probe, outputDir)

			tt.errFunc(t, err)
			if tt.check != nil {
				tt.check(t, outputDir, inputPath)
			}
		})
	}
}
