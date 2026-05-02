// Package steps provides workflow step implementations for the media processing workflows.
package steps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// StreamInfo holds the input stream index, language tag, and source title for
// one audio or subtitle stream.
type StreamInfo struct {
	Index    int    `json:"index"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`
}

// AudioStreamInfo holds stream info for an audio stream, including channel count.
type AudioStreamInfo struct {
	StreamInfo
	// ReportedChannelCount is the channel count as reported by ffprobe.
	// A value of 0 means the channel layout could not be detected.
	ReportedChannelCount int `json:"reported_channel_count"`
	// EffectiveChannelCount is the channel count used for processing decisions
	// such as downmix synthesis. When ReportedChannelCount is 0 (unknown layout),
	// this is set to a conservative surround value (6) so that unknown streams
	// are treated as surround rather than inadvertently blocking downmix synthesis.
	EffectiveChannelCount int  `json:"effective_channel_count"`
	HasLFE                bool `json:"has_lfe,omitempty"`
}

// ProbeOutput is the output of the probe step.
type ProbeOutput struct {
	// IsValidMedia is false when the file is not a recognisable media file with a
	// video stream. All downstream steps are skipped when this is false.
	IsValidMedia bool `json:"is_valid_media"`
	// VideoCodec is the codec name of the first video stream (e.g. "h264", "hevc").
	// Only meaningful when IsValidMedia is true.
	VideoCodec string `json:"video_codec"`
	// Format is the container format name as reported by ffprobe (e.g. "matroska,webm").
	// Only meaningful when IsValidMedia is true.
	Format string `json:"format"`
	// DurationSeconds is the total duration of the media file in seconds.
	// Only meaningful when IsValidMedia is true; zero otherwise.
	DurationSeconds float64 `json:"duration_seconds"`
	// AudioStreams lists every audio stream found in the file, in stream order.
	// Only meaningful when IsValidMedia is true.
	AudioStreams []AudioStreamInfo `json:"audio_streams,omitempty"`
	// SubtitleStreams lists every subtitle stream found in the file, in stream order.
	// Only meaningful when IsValidMedia is true.
	SubtitleStreams []StreamInfo `json:"subtitle_streams,omitempty"`
	// VideoWidth and VideoHeight are the pixel dimensions of the first video stream.
	// Only meaningful when IsValidMedia is true; zero otherwise.
	VideoWidth  int `json:"video_width,omitempty"`
	VideoHeight int `json:"video_height,omitempty"`
}

// RunProbe reads codec and container info for filePath. If the file is not a
// recognised media file or has no video stream, it deletes the file and returns
// IsValidMedia=false (without error), causing all downstream steps to be skipped.
// When retainEmptyDirs is false and a deletion occurs, empty parent directories up
// to watchRoot are also removed via pruneEmptyParents.
func RunProbe(ctx context.Context, filePath, watchRoot string, retainEmptyDirs bool) (ProbeOutput, error) {
	info, err := ffprobe.Probe(ctx, filePath)
	if err != nil {
		// Context errors are operational — propagate them so the step fails and
		// OnFailure fires, instead of silently deleting the file.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ProbeOutput{}, err
		}

		if removeErr := os.Remove(filePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return ProbeOutput{}, fmt.Errorf("remove unrecognised file: %w", removeErr)
		}

		if !retainEmptyDirs {
			pruneEmptyParents(filePath, watchRoot)
		}

		slog.WarnContext(ctx, "failed to probe file", "file", filePath, "error", err)

		return ProbeOutput{IsValidMedia: false}, nil
	}

	var (
		audioStreams    []AudioStreamInfo
		subtitleStreams []StreamInfo
	)

	for _, s := range info.Streams {
		switch s.CodecType {
		case ffprobe.CodecTypeAudio:
			reported := s.AudioChannelCount

			effective := reported
			if effective == 0 {
				// ffprobe reports 0 when the channel layout is unknown. Use a
				// conservative surround value so the downmix check does not
				// incorrectly suppress synthesis by matching the stereo threshold.
				effective = 6
			}

			audioStreams = append(audioStreams, AudioStreamInfo{
				StreamInfo:            StreamInfo{Index: s.Index, Language: s.Tags["language"], Title: s.Tags["title"]},
				ReportedChannelCount:  reported,
				EffectiveChannelCount: effective,
				HasLFE:                s.HasLFE,
			})
		case ffprobe.CodecTypeSubtitle:
			subtitleStreams = append(subtitleStreams, StreamInfo{
				Index:    s.Index,
				Language: s.Tags["language"],
				Title:    s.Tags["title"],
			})
		}
	}

	for _, s := range info.Streams {
		if s.CodecType == ffprobe.CodecTypeVideo && !isStillImageFormat(info.Format) {
			return ProbeOutput{
				IsValidMedia:    true,
				VideoCodec:      s.CodecName,
				Format:          info.Format,
				DurationSeconds: info.Duration.Seconds(),
				AudioStreams:    audioStreams,
				SubtitleStreams: subtitleStreams,
				VideoWidth:      s.WidthPixels,
				VideoHeight:     s.HeightPixels,
			}, nil
		}
	}

	// No video stream found.
	if removeErr := os.Remove(filePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return ProbeOutput{}, fmt.Errorf("remove file with no video streams: %w", removeErr)
	}

	if !retainEmptyDirs {
		pruneEmptyParents(filePath, watchRoot)
	}

	return ProbeOutput{IsValidMedia: false}, nil
}

// isStillImageFormat reports whether the FFmpeg format name identifies a
// still-image demuxer. These demuxers expose a codec_type=video stream but the
// file is not a motion-picture video container. All image-pipe demuxers carry a
// "_pipe" suffix (e.g. "png_pipe", "jpeg_pipe"); "image2" is the generic
// image-file/sequence demuxer.
func isStillImageFormat(format string) bool {
	return strings.HasSuffix(format, "_pipe") || format == "image2"
}
