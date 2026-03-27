package shared

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// mkvOutputName returns the expected output filename for a given input path:
// the input stem with ".mkv" extension.
func mkvOutputName(inputPath string) string {
	base := filepath.Base(inputPath)
	return strings.TrimSuffix(base, filepath.Ext(base)) + ".mkv"
}

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
			got := SelectVideoCodec(tt.videoCodecName, tt.format)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestNonEnglishAudioIndices(t *testing.T) {
	tests := []struct {
		name     string
		streams  []StreamInfo
		expected []int
	}{
		{
			name: "mixed languages with at least one eng keeps only eng streams",
			streams: []StreamInfo{
				{Index: 1, Language: "eng"},
				{Index: 2, Language: "jpn"},
				{Index: 3, Language: "fra"},
			},
			expected: []int{2, 3},
		},
		{
			name: "all eng streams returns empty exclusion list",
			streams: []StreamInfo{
				{Index: 1, Language: "eng"},
				{Index: 2, Language: "eng"},
			},
			expected: nil,
		},
		{
			name: "no language tags preserves all streams via safe fallback",
			streams: []StreamInfo{
				{Index: 1, Language: ""},
				{Index: 2, Language: ""},
			},
			expected: nil,
		},
		{
			name: "all non-eng languages preserves all streams via safe fallback",
			streams: []StreamInfo{
				{Index: 1, Language: "jpn"},
				{Index: 2, Language: "fra"},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nonEnglishAudioIndices(tt.streams)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestNonEnglishSubtitleIndices(t *testing.T) {
	tests := []struct {
		name     string
		streams  []StreamInfo
		expected []int
	}{
		{
			name: "mixed languages with at least one eng keeps only eng streams",
			streams: []StreamInfo{
				{Index: 2, Language: "eng"},
				{Index: 3, Language: "jpn"},
				{Index: 4, Language: "fra"},
			},
			expected: []int{3, 4},
		},
		{
			name: "all eng streams returns empty exclusion list",
			streams: []StreamInfo{
				{Index: 2, Language: "eng"},
				{Index: 3, Language: "eng"},
			},
			expected: nil,
		},
		{
			name: "no language tags removes all streams",
			streams: []StreamInfo{
				{Index: 2, Language: ""},
				{Index: 3, Language: ""},
			},
			expected: []int{2, 3},
		},
		{
			name: "no eng languages removes all streams",
			streams: []StreamInfo{
				{Index: 2, Language: "jpn"},
				{Index: 3, Language: "fra"},
			},
			expected: []int{2, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nonEnglishSubtitleIndices(tt.streams)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestFirstEnglishIndex(t *testing.T) {
	tests := []struct {
		name      string
		streams   []StreamInfo
		wantIdx   int
		wantFound bool
	}{
		{
			name: "first eng stream is returned when multiple languages are present",
			streams: []StreamInfo{
				{Index: 2, Language: "jpn"},
				{Index: 3, Language: "eng"},
				{Index: 4, Language: "eng"},
			},
			wantIdx:   3,
			wantFound: true,
		},
		{
			name:      "single eng stream is returned",
			streams:   []StreamInfo{{Index: 5, Language: "eng"}},
			wantIdx:   5,
			wantFound: true,
		},
		{
			name: "no eng stream returns not found",
			streams: []StreamInfo{
				{Index: 1, Language: "jpn"},
				{Index: 2, Language: "fra"},
			},
			wantFound: false,
		},
		{
			name:      "empty slice returns not found",
			streams:   nil,
			wantFound: false,
		},
		{
			name:      "untagged stream is not treated as English",
			streams:   []StreamInfo{{Index: 1, Language: ""}},
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIdx, gotFound := firstEnglishIndex(tt.streams)
			assert.Equal(t, tt.wantFound, gotFound)
			if tt.wantFound {
				assert.Equal(t, tt.wantIdx, gotIdx)
			}
		})
	}
}

func TestRunTranscode(t *testing.T) {
	// The fixture video has one audio stream (index 1) tagged "und" (undefined
	// language). nonEnglishAudioIndices returns nil when no "eng" stream is
	// present (safe fallback), so all streams are preserved.
	h264Probe := ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "mov,mp4,m4a,3gp,3g2,mj2",
		AudioStreams: []StreamInfo{{Index: 1, Language: "und"}},
	}

	tests := []struct {
		name    string
		setup   func(t *testing.T) (inputPath, outputDir string)
		probe   ProbeOutput
		errFunc require.ErrorAssertionFunc
		check   func(t *testing.T, outputDir, inputPath string)
	}{
		{
			name: "valid H.264 MP4 transcodes to output dir",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			probe:   h264Probe,
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				out := mkvOutputName(inputPath)
				_, err := os.Stat(filepath.Join(outputDir, out))
				require.NoError(t, err, "final output file should exist")

				_, statErr := os.Stat(filepath.Join(outputDir, "._"+out+".tmp"))
				assert.True(t, os.IsNotExist(statErr), "temp file should be removed after successful transcode")
			},
		},
		{
			name: "pre-existing temp file is overwritten",
			setup: func(t *testing.T) (string, string) {
				inputPath := copyTestVideo(t)
				outputDir := t.TempDir()
				stale := filepath.Join(outputDir, "._"+mkvOutputName(inputPath)+".tmp")
				require.NoError(t, os.WriteFile(stale, []byte("stale partial data"), 0o600))
				return inputPath, outputDir
			},
			probe:   h264Probe,
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				out := mkvOutputName(inputPath)
				_, err := os.Stat(filepath.Join(outputDir, out))
				require.NoError(t, err, "final output file should exist")

				_, statErr := os.Stat(filepath.Join(outputDir, "._"+out+".tmp"))
				assert.True(t, os.IsNotExist(statErr), "temp file should be removed after successful transcode")
			},
		},
		{
			name: "pre-existing final file is overwritten",
			setup: func(t *testing.T) (string, string) {
				inputPath := copyTestVideo(t)
				outputDir := t.TempDir()
				oldContent := []byte("old output")
				require.NoError(t, os.WriteFile(filepath.Join(outputDir, mkvOutputName(inputPath)), oldContent, 0o600))
				return inputPath, outputDir
			},
			probe:   h264Probe,
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				info, err := os.Stat(filepath.Join(outputDir, mkvOutputName(inputPath)))
				require.NoError(t, err, "final output file should exist")
				assert.Greater(t, info.Size(), int64(len("old output")), "final file should contain transcoded data, not old content")
			},
		},
		{
			name: "non-video input returns error and cleans up temp file",
			setup: func(t *testing.T) (string, string) {
				p := filepath.Join(t.TempDir(), "not-a-video.txt")
				require.NoError(t, os.WriteFile(p, []byte("not a video"), 0o600))
				return p, t.TempDir()
			},
			probe:   ProbeOutput{IsValidMedia: false},
			errFunc: require.Error,
			check: func(t *testing.T, outputDir, inputPath string) {
				_, statErr := os.Stat(filepath.Join(outputDir, "._"+mkvOutputName(inputPath)+".tmp"))
				assert.True(t, os.IsNotExist(statErr), "temp file should be cleaned up on transcode error")
			},
		},
		{
			name: "non-writable output directory returns error",
			setup: func(t *testing.T) (string, string) {
				if os.Getuid() == 0 {
					t.Skip("skipping permission test: chmod has no effect when running as root")
				}
				outputDir := t.TempDir()
				require.NoError(t, os.Chmod(outputDir, 0o555))
				t.Cleanup(func() { _ = os.Chmod(outputDir, 0o755) })
				return copyTestVideo(t), outputDir
			},
			probe:   h264Probe,
			errFunc: require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputPath, outputDir := tt.setup(t)

			err := RunTranscode(t.Context(), inputPath, tt.probe, outputDir)

			tt.errFunc(t, err)
			if tt.check != nil {
				tt.check(t, outputDir, inputPath)
			}
		})
	}
}
