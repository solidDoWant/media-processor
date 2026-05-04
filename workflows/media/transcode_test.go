package media

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// recordingHandler is a slog.Handler that captures log records for inspection in tests.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r.Clone())

	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordingHandler) progressRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []slog.Record

	for _, record := range h.records {
		if record.Message == "transcode progress" {
			out = append(out, record)
		}
	}

	return out
}

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
			name: "pre-existing non-media final file is overwritten",
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
			name: "pre-existing final file with matching duration is reused without re-encoding",
			setup: func(t *testing.T) (string, string) {
				inputPath := copyTestVideo(t)
				outputDir := t.TempDir()

				src, err := os.ReadFile(testVideoPath)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(outputDir, mkvOutputName(inputPath)), src, 0o600))

				return inputPath, outputDir
			},
			probe: ProbeOutput{
				IsValidMedia:    true,
				VideoCodec:      "h264",
				Format:          "mov,mp4,m4a,3gp,3g2,mj2",
				DurationSeconds: 5.013333,
				AudioStreams:    []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
			},
			errFunc: require.NoError,
			checkOutput: func(t *testing.T, out TranscodeOutput, inputPath string) {
				assert.Equal(t, "h264", out.DestCodec, "reused file's codec should be reported, not a fresh-transcode codec")
				assert.Equal(t, "mkv", out.DestContainer)
				assert.Zero(t, out.TranscodeDurationSeconds, "reuse path should report zero transcode duration since no encoding ran")
			},
			check: func(t *testing.T, outputDir, inputPath string) {
				finalContents, err := os.ReadFile(filepath.Join(outputDir, mkvOutputName(inputPath)))
				require.NoError(t, err, "final output file should exist")

				srcContents, err := os.ReadFile(testVideoPath)
				require.NoError(t, err)

				assert.Equal(t, srcContents, finalContents, "existing output should be preserved byte-for-byte when its duration matches")
			},
		},
		{
			name: "pre-existing final file with mismatched duration is overwritten",
			setup: func(t *testing.T) (string, string) {
				inputPath := copyTestVideo(t)
				outputDir := t.TempDir()

				src, err := os.ReadFile(testVideoPath)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(outputDir, mkvOutputName(inputPath)), src, 0o600))

				return inputPath, outputDir
			},
			probe: ProbeOutput{
				IsValidMedia:    true,
				VideoCodec:      "h264",
				Format:          "mov,mp4,m4a,3gp,3g2,mj2",
				DurationSeconds: 100.0,
				AudioStreams:    []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
			},
			errFunc: require.NoError,
			checkOutput: func(t *testing.T, out TranscodeOutput, inputPath string) {
				assert.Equal(t, "hevc", out.DestCodec, "duration mismatch should trigger a fresh H.265 transcode")
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

			out, err := RunTranscode(t.Context(), TranscodeRequest{
				FilePath:  inputPath,
				Probe:     tt.probe,
				OutputDir: outputDir,
			})

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

	out, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:    inputPath,
		Probe:       probe,
		OutputDir:   outputDir,
		WatcherRoot: watcherRoot,
	})
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

	out, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:    inputPath,
		Probe:       probe,
		OutputDir:   outputDir,
		WatcherRoot: watcherRoot,
	})
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

	_, err = RunTranscode(t.Context(), TranscodeRequest{
		FilePath:    inputPath,
		Probe:       probe,
		OutputDir:   outputDir,
		WatcherRoot: watcherRoot,
	})
	require.Error(t, err, "input outside watcherRoot should return an error")

	entries, readErr := os.ReadDir(outputDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no output files or subdirs should be created when input is outside watcherRoot")
}

// progressProbe returns a minimal ProbeOutput suitable for progress logging tests.
func progressProbe() ProbeOutput {
	return ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "mov,mp4,m4a,3gp,3g2,mj2",
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
	}
}

// withRecordingLogger replaces the default slog logger for the duration of the test
// and returns the recording handler. The original logger is restored by t.Cleanup.
func withRecordingLogger(t *testing.T) *recordingHandler {
	t.Helper()

	handler := &recordingHandler{}
	orig := slog.Default()

	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(orig) })

	return handler
}

func TestRunTranscode_ProgressLogging_EmitsLinesAtInterval(t *testing.T) {
	handler := withRecordingLogger(t)

	_, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:            copyTestVideo(t),
		Probe:               progressProbe(),
		OutputDir:           t.TempDir(),
		ProgressLogInterval: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	assert.Eventually(t,
		func() bool { return len(handler.progressRecords()) > 1 },
		time.Second, time.Millisecond,
		"expected multiple progress log lines with a 50ms interval, not just the final completion log",
	)

	records := handler.progressRecords()
	require.NotEmpty(t, records)

	first := records[0]
	keys := map[string]struct{}{}

	first.Attrs(func(a slog.Attr) bool {
		keys[a.Key] = struct{}{}
		return true
	})

	assert.Contains(t, keys, "percent_complete")
	assert.Contains(t, keys, "elapsed")
	assert.Contains(t, keys, "frames_processed")
	assert.Contains(t, keys, "fps")
}

func TestRunTranscode_ProgressLogging_NoLinesWhenDisabled(t *testing.T) {
	handler := withRecordingLogger(t)

	_, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:  copyTestVideo(t),
		Probe:     progressProbe(),
		OutputDir: t.TempDir(),
	})
	require.NoError(t, err)

	assert.Empty(t, handler.progressRecords(), "expected no progress log lines when interval is zero")
}

func TestRunTranscode_ProgressLogging_FinalLineEmittedOnCompletion(t *testing.T) {
	handler := withRecordingLogger(t)

	// Interval longer than the transcode so no tick fires; the final log on done must appear.
	_, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:            copyTestVideo(t),
		Probe:               progressProbe(),
		OutputDir:           t.TempDir(),
		ProgressLogInterval: time.Hour,
	})
	require.NoError(t, err)

	// The goroutine emits its final log after RunTranscode returns; poll briefly for it.
	assert.Eventually(t,
		func() bool { return len(handler.progressRecords()) > 0 },
		time.Second, time.Millisecond,
		"expected one final progress log line even when no tick fired during the transcode",
	)
}

func TestRunTranscode_Heartbeat_InvokedOnProgressTicks(t *testing.T) {
	withRecordingLogger(t)

	var (
		mu    sync.Mutex
		ticks []ffmpeg.Progress
	)

	heartbeat := func(p ffmpeg.Progress) {
		mu.Lock()
		defer mu.Unlock()

		ticks = append(ticks, p)
	}

	_, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:            copyTestVideo(t),
		Probe:               progressProbe(),
		OutputDir:           t.TempDir(),
		ProgressLogInterval: 50 * time.Millisecond,
		Heartbeat:           heartbeat,
	})
	require.NoError(t, err)

	// Poll for the final synthetic 100% tick. Real progress ticks land first
	// (with whatever percent_complete the encoder reports), and the closing
	// 100% tick is dispatched by the goroutine after RunTranscode returns.
	assert.Eventually(t,
		func() bool {
			mu.Lock()
			defer mu.Unlock()

			if len(ticks) == 0 {
				return false
			}

			return ticks[len(ticks)-1].PercentComplete >= 100
		},
		time.Second, time.Millisecond,
		"expected the heartbeat callback to be invoked, ending with the final 100%% closing tick",
	)

	mu.Lock()
	defer mu.Unlock()

	assert.Greater(t, len(ticks), 1,
		"expected multiple heartbeat invocations across real progress ticks plus the final 100%% tick")
}

func TestRunTranscode_Heartbeat_FiresOnCopyRemuxPathWithoutProgressPackets(t *testing.T) {
	withRecordingLogger(t)

	var (
		mu    sync.Mutex
		ticks []ffmpeg.Progress
	)

	heartbeat := func(p ffmpeg.Progress) {
		mu.Lock()
		defer mu.Unlock()

		ticks = append(ticks, p)
	}

	// H.264 in MKV → CodecCopy: ffmpeg emits no progress packets.
	// Heartbeats must still fire from the goroutine's ticker so that a
	// healthy long-running copy does not silently exceed HeartbeatTimeout.
	copyProbe := ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "matroska,webm",
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
	}

	// Small interval so the ticker is guaranteed to fire at least once
	// during the copy. The Temporal SDK throttles heartbeats internally,
	// so calling the callback frequently is harmless.
	_, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:            copyTestVideo(t),
		Probe:               copyProbe,
		OutputDir:           t.TempDir(),
		ProgressLogInterval: time.Millisecond,
		Heartbeat:           heartbeat,
	})
	require.NoError(t, err)

	// The final 100% tick is dispatched by the goroutine on `done`, which
	// runs after RunTranscode returns. Poll for it.
	assert.Eventually(t,
		func() bool {
			mu.Lock()
			defer mu.Unlock()

			if len(ticks) == 0 {
				return false
			}

			return ticks[len(ticks)-1].PercentComplete >= 100
		},
		time.Second, time.Millisecond,
		"expected a final 100%% heartbeat on the copy/remux path",
	)

	mu.Lock()
	defer mu.Unlock()

	// Without the ticker-driven heartbeat, the only invocation on the
	// copy/remux path would be the final synthetic 100% tick. Requiring
	// more than one invocation proves the ticker also fires heartbeats
	// while the copy is in flight.
	assert.Greater(t, len(ticks), 1,
		"expected the goroutine's ticker to record heartbeats even when ffmpeg emits no progress packets (copy/remux path)")
}

func TestRunTranscode_Heartbeat_NotInvokedWhenProgressDisabled(t *testing.T) {
	withRecordingLogger(t)

	var called bool

	heartbeat := func(_ ffmpeg.Progress) {
		called = true
	}

	_, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:  copyTestVideo(t),
		Probe:     progressProbe(),
		OutputDir: t.TempDir(),
		Heartbeat: heartbeat,
	})
	require.NoError(t, err)

	assert.False(t, called, "heartbeat must not be invoked when progress logging is disabled (interval == 0)")
}

func TestRunTranscode_ProgressLogging_CopyPathReports100Percent(t *testing.T) {
	handler := withRecordingLogger(t)

	// H.264 in MKV causes SelectVideoCodec to return CodecCopy: no re-encode, so ffmpeg
	// never sends progress updates. The final log must still report 100% completion.
	copyProbe := ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "matroska,webm",
		AudioStreams: []AudioStreamInfo{audioStreamInfo(1, "und", 2)},
	}

	_, err := RunTranscode(t.Context(), TranscodeRequest{
		FilePath:            copyTestVideo(t),
		Probe:               copyProbe,
		OutputDir:           t.TempDir(),
		ProgressLogInterval: time.Hour,
	})
	require.NoError(t, err)

	assert.Eventually(t,
		func() bool { return len(handler.progressRecords()) > 0 },
		time.Second, time.Millisecond,
		"expected a final progress log line on copy/remux path",
	)

	records := handler.progressRecords()
	require.Len(t, records, 1, "expected exactly one log line on copy/remux path (final only, no ticks)")

	var percentComplete float64

	records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "percent_complete" {
			percentComplete = a.Value.Float64()
		}

		return true
	})

	assert.InDelta(t, 100.0, percentComplete, 0.001, "copy/remux final log must report 100%% completion")
}
