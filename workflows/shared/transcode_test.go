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

// audioStreamInfo is a test helper that builds an AudioStreamInfo with the given fields.
func audioStreamInfo(index int, lang string, channels int) AudioStreamInfo {
	return AudioStreamInfo{StreamInfo: StreamInfo{Index: index, Language: lang}, ChannelCount: channels}
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
		streams  []AudioStreamInfo
		expected []int
	}{
		{
			name: "mixed languages with at least one eng keeps only eng streams",
			streams: []AudioStreamInfo{
				audioStreamInfo(1, "eng", 2),
				audioStreamInfo(2, "jpn", 2),
				audioStreamInfo(3, "fra", 2),
			},
			expected: []int{2, 3},
		},
		{
			name: "all eng streams returns empty exclusion list",
			streams: []AudioStreamInfo{
				audioStreamInfo(1, "eng", 2),
				audioStreamInfo(2, "eng", 2),
			},
			expected: nil,
		},
		{
			name: "no language tags preserves all streams via safe fallback",
			streams: []AudioStreamInfo{
				audioStreamInfo(1, "", 2),
				audioStreamInfo(2, "", 2),
			},
			expected: nil,
		},
		{
			name: "all non-eng languages preserves all streams via safe fallback",
			streams: []AudioStreamInfo{
				audioStreamInfo(1, "jpn", 2),
				audioStreamInfo(2, "fra", 2),
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
	intPtr := func(i int) *int { return &i }
	tests := []struct {
		name    string
		streams []StreamInfo
		want    *int
	}{
		{
			name: "first eng stream is returned when multiple languages are present",
			streams: []StreamInfo{
				{Index: 2, Language: "jpn"},
				{Index: 3, Language: "eng"},
				{Index: 4, Language: "eng"},
			},
			want: intPtr(3),
		},
		{
			name:    "single eng stream is returned",
			streams: []StreamInfo{{Index: 5, Language: "eng"}},
			want:    intPtr(5),
		},
		{
			name: "no eng stream returns nil",
			streams: []StreamInfo{
				{Index: 1, Language: "jpn"},
				{Index: 2, Language: "fra"},
			},
			want: nil,
		},
		{
			name:    "empty slice returns nil",
			streams: nil,
			want:    nil,
		},
		{
			name:    "untagged stream is not treated as English",
			streams: []StreamInfo{{Index: 1, Language: ""}},
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstEnglishIndex(tt.streams)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDownmixSourceIndex(t *testing.T) {
	intPtr := func(i int) *int { return &i }
	tests := []struct {
		name    string
		streams []AudioStreamInfo
		want    *int
	}{
		{
			name:    "empty slice returns nil",
			streams: nil,
			want:    nil,
		},
		{
			name:    "single stereo stream returns nil (stereo-compatible present)",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 2)},
			want:    nil,
		},
		{
			name:    "stereo-compatible stream among surround returns nil",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 6), audioStreamInfo(2, "eng", 2)},
			want:    nil,
		},
		{
			name:    "single surround stream returns its index",
			streams: []AudioStreamInfo{audioStreamInfo(3, "eng", 6)},
			want:    intPtr(3),
		},
		{
			name: "multiple surround streams returns first index",
			streams: []AudioStreamInfo{
				audioStreamInfo(2, "eng", 6),
				audioStreamInfo(4, "eng", 8),
			},
			want: intPtr(2),
		},
		{
			name:    "mono stream (1 channel) is stereo-compatible, returns nil",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 1)},
			want:    nil,
		},
		{
			name:    "3-channel stream is stereo-compatible, returns nil",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 3)},
			want:    nil,
		},
		{
			name:    "4-channel stream triggers downmix",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 4)},
			want:    intPtr(1),
		},
		{
			name:    "zero channel count (unknown layout) is skipped, not treated as stereo-compatible",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 0), audioStreamInfo(2, "eng", 6)},
			want:    intPtr(2),
		},
		{
			name:    "all streams with zero channel count return nil (no known surround)",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 0)},
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := downmixSourceIndex(tt.streams)
			assert.Equal(t, tt.want, got)
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
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
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
		{
			name: "surround-only probe triggers downmix stream in output",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			// Report the fixture audio as surround so downmixSourceIndex returns non-nil.
			probe: ProbeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
				AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 6)},
			},
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				out := filepath.Join(outputDir, mkvOutputName(inputPath))
				info, err := ffprobe.Probe(t.Context(), out)
				require.NoError(t, err)
				var audioCount int
				for _, s := range info.Streams {
					if s.CodecType == ffprobe.CodecTypeAudio {
						audioCount++
					}
				}
				assert.Equal(t, 2, audioCount, "output should contain original audio stream plus one downmixed stream")
			},
		},
		{
			name: "stereo audio stream title is set to channel config label in output",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			// Stereo audio (2 channels, no LFE): expected title "2.0".
			probe: ProbeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
				AudioStreams: []AudioStreamInfo{
					{StreamInfo: StreamInfo{Index: 1, Language: "und"}, ChannelCount: 2, HasLFE: false},
				},
			},
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				out := filepath.Join(outputDir, mkvOutputName(inputPath))
				info, err := ffprobe.Probe(t.Context(), out)
				require.NoError(t, err)
				for _, s := range info.Streams {
					if s.CodecType == ffprobe.CodecTypeAudio {
						assert.Equal(t, "2.0", s.Tags["title"], "stereo audio stream should have title '2.0'")
						return
					}
				}
				t.Fatal("no audio stream found in output")
			},
		},
		{
			name: "downmix stream title is derived from actual encoder channel layout",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			// Report as surround so a downmix is synthesized.
			probe: ProbeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
				AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 6)},
			},
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				out := filepath.Join(outputDir, mkvOutputName(inputPath))
				info, err := ffprobe.Probe(t.Context(), out)
				require.NoError(t, err)
				// The downmix stream is the last audio stream. Its title must be
				// "2.1" when the AC-3 encoder supports the 2.1 layout, or "2.0"
				// when it falls back to stereo.
				var lastAudio ffprobe.StreamInfo
				for _, s := range info.Streams {
					if s.CodecType == ffprobe.CodecTypeAudio {
						lastAudio = s
					}
				}
				title := lastAudio.Tags["title"]
				assert.True(t, title == "2.1" || title == "2.0",
					"downmix stream title should be '2.1' or '2.0', got %q", title)
			},
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
