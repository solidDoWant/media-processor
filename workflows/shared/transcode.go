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
func nonEnglishAudioIndices(streams []StreamInfo) []int {
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

// RunTranscode transcodes filePath into outputDir, writing to a temp file named
// "._<stem>.mkv.tmp" and atomically renaming it to "<stem>.mkv" on success.
// The output always carries a .mkv extension to match the forced MKV container.
// Writing directly to the output directory avoids a cross-filesystem copy and
// guarantees the rename is atomic on Linux (same directory).
// probe is the output of RunProbe for filePath.
func RunTranscode(ctx context.Context, filePath string, probe ProbeOutput, outputDir string) error {
	videoCodec := SelectVideoCodec(probe.VideoCodec, probe.Format)
	excludeIndices := append(nonEnglishAudioIndices(probe.AudioStreams), nonEnglishSubtitleIndices(probe.SubtitleStreams)...)

	inputBase := filepath.Base(filePath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	tempPath := filepath.Join(outputDir, "._"+mkvBase+".tmp")
	finalPath := filepath.Join(outputDir, mkvBase)

	if err := ffmpeg.NewTranscode(filePath, tempPath).
		ToVideoCodec(videoCodec).
		ToContainer(ffmpeg.ContainerMKV).
		ExcludeStreams(excludeIndices...).
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
