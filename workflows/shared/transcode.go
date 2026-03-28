package shared

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// SelectVideoCodec returns CodecCopy when the video is already H.264 or H.265 in an
// MKV container, and CodecH265 otherwise.
func SelectVideoCodec(videoCodecName, format string) ffmpeg.Codec {
	if strings.Contains(format, string(ffmpeg.ContainerMKV)) {
		if videoCodecName == ffprobe.CodecNameH264 || videoCodecName == ffprobe.CodecNameH265 {
			return ffmpeg.CodecCopy
		}
	}

	return ffmpeg.CodecH265
}

// nonEnglishAudioIndices returns the input stream indices of non-English audio
// streams. If at least one stream carries the "eng" language tag, all streams
// without that tag are returned so they can be excluded from the output.
// If no stream is tagged "eng" (including the case where no streams carry any
// language tag), nil is returned so all streams are preserved as a safe fallback.
func nonEnglishAudioIndices(streams []AudioStreamInfo) []int {
	hasEnglish := false
	for _, s := range streams {
		if s.Language == "eng" {
			hasEnglish = true
			break
		}
	}
	if !hasEnglish {
		return nil
	}
	var exclude []int
	for _, s := range streams {
		if s.Language != "eng" {
			exclude = append(exclude, s.Index)
		}
	}
	return exclude
}

// downmixSourceIndex returns a pointer to the input stream Index of the first
// AudioStreamInfo element with EffectiveChannelCount >= 4, when no retained audio
// stream has EffectiveChannelCount <= 3 (stereo-compatible). Returns nil if any
// stream is stereo-compatible, if no surround stream exists, or if the slice is
// empty. Streams with unknown layouts have EffectiveChannelCount set to a
// conservative surround value by RunProbe, so they are treated as surround
// candidates without blocking or triggering synthesis incorrectly.
func downmixSourceIndex(streams []AudioStreamInfo) *int {
	var firstSurround *int
	for _, s := range streams {
		if s.EffectiveChannelCount > 0 && s.EffectiveChannelCount <= 3 {
			return nil
		}
		if s.EffectiveChannelCount >= 4 && firstSurround == nil {
			idx := s.Index
			firstSurround = &idx
		}
	}
	return firstSurround
}

// nonEnglishSubtitleIndices returns the input stream indices of all subtitle
// streams not tagged "eng", including untagged streams. Unlike audio, there is
// no safe-fallback: subtitles are always excluded unless explicitly tagged "eng".
func nonEnglishSubtitleIndices(streams []StreamInfo) []int {
	var exclude []int
	for _, s := range streams {
		if s.Language != "eng" {
			exclude = append(exclude, s.Index)
		}
	}
	return exclude
}

// firstEnglishIndex returns a pointer to the input stream Index of the first
// StreamInfo element tagged "eng", or nil when no English stream is found.
func firstEnglishIndex(streams []StreamInfo) *int {
	for _, stream := range streams {
		if stream.Language == "eng" {
			return &stream.Index
		}
	}
	return nil
}

// RunTranscode transcodes filePath into outputDir, writing to a temp file named
// "._<stem>.mkv.tmp" and atomically renaming it to "<stem>.mkv" on success.
// The output always carries a .mkv extension to match the forced MKV container.
// Writing directly to the output directory avoids a cross-filesystem copy and
// guarantees the rename is atomic on Linux (same directory).
// probe is the output of RunProbe for filePath.
func RunTranscode(ctx context.Context, filePath string, probe ProbeOutput, outputDir string) error {
	videoCodec := SelectVideoCodec(probe.VideoCodec, probe.Format)

	audioExclude := nonEnglishAudioIndices(probe.AudioStreams)
	excludeIndices := append(audioExclude, nonEnglishSubtitleIndices(probe.SubtitleStreams)...)

	// Compute retained audio streams to decide whether a downmix is needed.
	excludeSet := make(map[int]bool, len(audioExclude))
	for _, idx := range audioExclude {
		excludeSet[idx] = true
	}
	retainedAudio := make([]AudioStreamInfo, 0, len(probe.AudioStreams))
	for _, s := range probe.AudioStreams {
		if !excludeSet[s.Index] {
			retainedAudio = append(retainedAudio, s)
		}
	}

	// Extract base StreamInfo slice for generic functions (firstEnglishIndex).
	audioBaseStreams := make([]StreamInfo, len(probe.AudioStreams))
	for i, s := range probe.AudioStreams {
		audioBaseStreams[i] = s.StreamInfo
	}

	inputBase := filepath.Base(filePath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	tempPath := filepath.Join(outputDir, "._"+mkvBase+".tmp")
	finalPath := filepath.Join(outputDir, mkvBase)

	// Build per-stream title map for retained audio streams.
	// Streams with unknown channel layouts (ReportedChannelCount=0) still receive
	// a language-only title when a language tag is present; otherwise they are skipped.
	audioTitles := make(map[int]string, len(retainedAudio))
	for _, s := range retainedAudio {
		langName := iso639Name(s.Language)
		if s.ReportedChannelCount == 0 {
			if langName != "" {
				audioTitles[s.Index] = langName
			}
			continue
		}
		label := channelConfigLabel(s.ReportedChannelCount, s.HasLFE)
		audioTitles[s.Index] = buildAudioStreamTitle(s.Title, langName, label)
	}

	// Build per-stream title map for retained subtitle streams.
	subtitleExcludeSet := make(map[int]bool, len(nonEnglishSubtitleIndices(probe.SubtitleStreams)))
	for _, idx := range nonEnglishSubtitleIndices(probe.SubtitleStreams) {
		subtitleExcludeSet[idx] = true
	}
	subtitleTitles := make(map[int]string, len(probe.SubtitleStreams))
	for _, s := range probe.SubtitleStreams {
		if subtitleExcludeSet[s.Index] {
			continue
		}
		title := buildSubtitleStreamTitle(s.Title, iso639Name(s.Language))
		if title != "" {
			subtitleTitles[s.Index] = title
		}
	}

	// Resolve the language name for the downmix source stream so the downmix
	// title can be prefixed with the human-readable language name.
	downmixSrcIdx := downmixSourceIndex(retainedAudio)
	var downmixLangName string
	if downmixSrcIdx != nil {
		for _, s := range retainedAudio {
			if s.Index == *downmixSrcIdx {
				downmixLangName = iso639Name(s.Language)
				break
			}
		}
	}

	if err := ffmpeg.NewTranscode(filePath, tempPath).
		ToVideoCodec(videoCodec).
		ToContainer(ffmpeg.ContainerMKV).
		ExcludeStreams(excludeIndices...).
		WithDefaultAudioStream(firstEnglishIndex(audioBaseStreams)).
		WithDefaultSubtitleStream(firstEnglishIndex(probe.SubtitleStreams)).
		WithDownmix(downmixSrcIdx).
		WithAudioStreamTitles(audioTitles).
		WithSubtitleStreamTitles(subtitleTitles).
		WithDownmixTitle(downmixLangName).
		WithAutoDownmixTitle().
		Build().
		Run(ctx); err != nil {
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("transcode: %w", err),
				fmt.Errorf("cleanup temp file: %w", removeErr),
			)
		}

		return fmt.Errorf("transcode: %w", err)
	}

	if err := os.Rename(tempPath, finalPath); err != nil {
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("move output file: %w", err),
				fmt.Errorf("cleanup temp file: %w", removeErr),
			)
		}

		return fmt.Errorf("move output file: %w", err)
	}

	return nil
}
