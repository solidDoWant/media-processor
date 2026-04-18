package steps

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
// EffectiveChannelCount is coerced to 6 when channels is 0, matching RunProbe behaviour
// for streams with unknown channel layouts.
func audioStreamInfo(index int, lang string, channels int) AudioStreamInfo {
	effective := channels
	if effective == 0 {
		effective = 6
	}

	return AudioStreamInfo{
		StreamInfo:            StreamInfo{Index: index, Language: lang},
		ReportedChannelCount:  channels,
		EffectiveChannelCount: effective,
	}
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
			name:    "zero channel count (unknown layout) is treated as surround candidate, first index returned",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 0), audioStreamInfo(2, "eng", 6)},
			want:    intPtr(1),
		},
		{
			name:    "stream with zero channel count (unknown layout) is treated as surround candidate",
			streams: []AudioStreamInfo{audioStreamInfo(1, "eng", 0)},
			want:    intPtr(1),
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

	// mkvH264Probe simulates an H.264 file already in MKV — SelectVideoCodec returns CodecCopy.
	mkvH264Probe := ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "matroska,webm",
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
	}

	tests := []struct {
		name        string
		setup       func(t *testing.T) (inputPath, outputDir string)
		probe       ProbeOutput
		errFunc     require.ErrorAssertionFunc
		checkOutput func(t *testing.T, out TranscodeOutput, inputPath string)
		check       func(t *testing.T, outputDir, inputPath string)
	}{
		{
			name: "valid H.264 MP4 transcodes to output dir",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			probe:   h264Probe,
			errFunc: require.NoError,
			checkOutput: func(t *testing.T, out TranscodeOutput, inputPath string) {
				assert.Equal(t, "hevc", out.DestCodec, "H.264 MP4 should be transcoded to hevc")
				assert.Equal(t, "mkv", out.DestContainer)
				assert.NotEmpty(t, out.DestFilePath)
				assert.Greater(t, out.SourceFileSizeBytes, int64(0), "source file size should be positive")
				assert.Greater(t, out.DestFileSizeBytes, int64(0), "dest file size should be positive")
			},
			check: func(t *testing.T, outputDir, inputPath string) {
				out := mkvOutputName(inputPath)
				_, err := os.Stat(filepath.Join(outputDir, out))
				require.NoError(t, err, "final output file should exist")

				_, statErr := os.Stat(filepath.Join(outputDir, "._"+out+".tmp"))
				assert.True(t, os.IsNotExist(statErr), "temp file should be removed after successful transcode")
			},
		},
		{
			name: "H.264 in MKV container is stream-copied and TranscodeOutput reflects copy codec",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			probe:   mkvH264Probe,
			errFunc: require.NoError,
			checkOutput: func(t *testing.T, out TranscodeOutput, inputPath string) {
				assert.Equal(t, "copy", out.DestCodec, "H.264 in MKV should use copy codec")
				assert.Equal(t, "mkv", out.DestContainer)
				assert.NotEmpty(t, out.DestFilePath)
				assert.Greater(t, out.SourceFileSizeBytes, int64(0))
				assert.Greater(t, out.DestFileSizeBytes, int64(0))
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
			name: "single-language audio tracks omit language from title",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			// All streams share the same language ("und"), so language is suppressed
			// and the title is just the channel config label.
			probe: ProbeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
				AudioStreams: []AudioStreamInfo{
					{StreamInfo: StreamInfo{Index: 1, Language: "und"}, ReportedChannelCount: 2, EffectiveChannelCount: 2, HasLFE: false},
				},
			},
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				out := filepath.Join(outputDir, mkvOutputName(inputPath))
				info, err := ffprobe.Probe(t.Context(), out)
				require.NoError(t, err)

				for _, s := range info.Streams {
					if s.CodecType == ffprobe.CodecTypeAudio {
						assert.Equal(t, "2.0", s.Tags["title"],
							"single-language stereo stream should have title '2.0' with no language prefix")

						return
					}
				}

				t.Fatal("no audio stream found in output")
			},
		},
		{
			name: "multi-language audio tracks include language in title, und shown as Unknown Language",
			setup: func(t *testing.T) (string, string) {
				// Use the two-audio fixture: stream 1 = AAC stereo "jpn",
				// stream 2 = AAC stereo "und".
				return copyTwoAudioTestVideo(t), t.TempDir()
			},
			// "jpn" + "und": nonEnglishAudioIndices only filters when English
			// is present, so both streams are retained here.
			probe: ProbeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
				AudioStreams: []AudioStreamInfo{
					{StreamInfo: StreamInfo{Index: 1, Language: "jpn"}, ReportedChannelCount: 2, EffectiveChannelCount: 2, HasLFE: false},
					{StreamInfo: StreamInfo{Index: 2, Language: "und"}, ReportedChannelCount: 2, EffectiveChannelCount: 2, HasLFE: false},
				},
			},
			errFunc: require.NoError,
			check: func(t *testing.T, outputDir, inputPath string) {
				out := filepath.Join(outputDir, mkvOutputName(inputPath))
				info, err := ffprobe.Probe(t.Context(), out)
				require.NoError(t, err)

				var titles []string

				for _, s := range info.Streams {
					if s.CodecType == ffprobe.CodecTypeAudio {
						titles = append(titles, s.Tags["title"])
					}
				}

				require.Len(t, titles, 2, "expected two audio streams")
				assert.Equal(t, "Japanese 2.0", titles[0], "Japanese stereo stream title")
				assert.Equal(t, "Unknown Language 2.0", titles[1], "undetermined stereo stream title")
			},
		},
		{
			name: "single-language surround-only probe omits language from downmix title",
			setup: func(t *testing.T) (string, string) {
				return copyTestVideo(t), t.TempDir()
			},
			// All streams are "und"; downmix title should be just the channel layout
			// label with no language prefix.
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

				var lastAudio ffprobe.StreamInfo

				for _, s := range info.Streams {
					if s.CodecType == ffprobe.CodecTypeAudio {
						lastAudio = s
					}
				}

				title := lastAudio.Tags["title"]
				assert.True(t, title == "2.1" || title == "2.0",
					"downmix stream title should be '2.1' or '2.0' with no language prefix, got %q", title)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputPath, outputDir := tt.setup(t)

			out, err := RunTranscode(t.Context(), inputPath, tt.probe, nil, outputDir, "", "", 0, nil)

			tt.errFunc(t, err)

			if tt.checkOutput != nil {
				tt.checkOutput(t, out, inputPath)
			}

			if tt.check != nil {
				tt.check(t, outputDir, inputPath)
			}
		})
	}
}

func TestRunTranscode_WatcherRoot_SubdirIsPreservedInOutput(t *testing.T) {
	watcherRoot := t.TempDir()
	subdir := filepath.Join(watcherRoot, "my-media-item")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	src, err := os.ReadFile(testVideoPath)
	require.NoError(t, err)

	inputPath := filepath.Join(subdir, "video.mp4")
	require.NoError(t, os.WriteFile(inputPath, src, 0o600))

	outputDir := t.TempDir()

	probe := ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "mov,mp4,m4a,3gp,3g2,mj2",
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
	}

	out, err := RunTranscode(t.Context(), inputPath, probe, nil, outputDir, watcherRoot, "", 0, nil)
	require.NoError(t, err)

	expectedPath := filepath.Join(outputDir, "my-media-item", "video.mkv")
	assert.Equal(t, expectedPath, out.DestFilePath)

	_, statErr := os.Stat(expectedPath)
	require.NoError(t, statErr, "output file should exist under the mirrored subdirectory")

	_, statErr = os.Stat(filepath.Join(outputDir, "video.mkv"))
	assert.True(t, os.IsNotExist(statErr), "output file should not be written flat into outputDir")
}

func TestRunTranscode_WatcherRoot_FlatInputProducesFlatOutput(t *testing.T) {
	watcherRoot := t.TempDir()

	src, err := os.ReadFile(testVideoPath)
	require.NoError(t, err)

	inputPath := filepath.Join(watcherRoot, "video.mp4")
	require.NoError(t, os.WriteFile(inputPath, src, 0o600))

	outputDir := t.TempDir()

	probe := ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "mov,mp4,m4a,3gp,3g2,mj2",
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
	}

	out, err := RunTranscode(t.Context(), inputPath, probe, nil, outputDir, watcherRoot, "", 0, nil)
	require.NoError(t, err)

	expectedPath := filepath.Join(outputDir, "video.mkv")
	assert.Equal(t, expectedPath, out.DestFilePath)

	_, statErr := os.Stat(expectedPath)
	require.NoError(t, statErr, "output file should be written directly in outputDir when input is at watcher root")
}

func TestRunTranscode_WatcherRoot_InputOutsideWatcherRootReturnsError(t *testing.T) {
	watcherRoot := t.TempDir()
	outsideDir := t.TempDir() // separate temp dir, not under watcherRoot

	src, err := os.ReadFile(testVideoPath)
	require.NoError(t, err)

	inputPath := filepath.Join(outsideDir, "video.mp4")
	require.NoError(t, os.WriteFile(inputPath, src, 0o600))

	outputDir := t.TempDir()

	probe := ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "mov,mp4,m4a,3gp,3g2,mj2",
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
	}

	_, err = RunTranscode(t.Context(), inputPath, probe, nil, outputDir, watcherRoot, "", 0, nil)
	require.Error(t, err, "input outside watcherRoot should return an error")

	entries, readErr := os.ReadDir(outputDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no output files or subdirs should be created when input is outside watcherRoot")
}
