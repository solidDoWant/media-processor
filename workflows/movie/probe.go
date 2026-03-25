package movie

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// probeOutput is the output of the probe step.
type probeOutput struct {
	// IsValidMedia is false when the file is not a recognisable media file with a
	// video stream. All downstream steps are skipped when this is false.
	IsValidMedia bool `json:"is_valid_media"`
	// VideoCodec is the codec name of the first video stream (e.g. "h264", "hevc").
	// Only meaningful when IsValidMedia is true.
	VideoCodec string `json:"video_codec"`
	// Format is the container format name as reported by ffprobe (e.g. "matroska,webm").
	// Only meaningful when IsValidMedia is true.
	Format string `json:"format"`
}

// runProbe reads codec and container info for filePath. If the file is not a
// recognised media file or has no video stream, it deletes the file and returns
// IsValidMedia=false (without error), causing all downstream steps to be skipped.
func runProbe(ctx context.Context, filePath string) (probeOutput, error) {
	info, err := ffprobe.Probe(ctx, filePath)
	if err != nil {
		if removeErr := os.Remove(filePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return probeOutput{}, fmt.Errorf("remove unrecognised file: %w", removeErr)
		}

		return probeOutput{IsValidMedia: false}, nil
	}

	for _, s := range info.Streams {
		if s.CodecType == ffprobe.CodecTypeVideo {
			return probeOutput{
				IsValidMedia: true,
				VideoCodec:   s.CodecName,
				Format:       info.Format,
			}, nil
		}
	}

	// No video stream found — not a movie file.
	if removeErr := os.Remove(filePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return probeOutput{}, fmt.Errorf("remove file with no video streams: %w", removeErr)
	}

	return probeOutput{IsValidMedia: false}, nil
}
