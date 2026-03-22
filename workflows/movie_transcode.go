package workflows

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

// transcodeOutput is the output of the transcode step.
type transcodeOutput struct {
	// TempPath is the path of the transcoded file within a workflow-run-specific
	// subdirectory of the system temp directory.
	TempPath string `json:"temp_path"`
}

// selectVideoCodec returns CodecCopy when the video is already H.264 or H.265 in an
// MKV container, and CodecH265 otherwise.
func selectVideoCodec(videoCodecName, format string) ffmpeg.Codec {
	if strings.Contains(format, string(ffmpeg.ContainerMKV)) {
		if videoCodecName == ffprobe.CodecNameH264 || videoCodecName == ffprobe.CodecNameH265 {
			return ffmpeg.CodecCopy
		}
	}
	return ffmpeg.CodecH265
}

// runTranscode transcodes the file at input.FilePath, writing output to a
// workflow-run-specific subdirectory of the system temp directory. The caller
// is responsible for eventually removing the directory (e.g. via runMove).
func runTranscode(ctx context.Context, input MovieInput, probe probeOutput, runID string) (transcodeOutput, error) {
	videoCodec := selectVideoCodec(probe.VideoCodec, probe.Format)

	tempDir := filepath.Join(os.TempDir(), "media-processor-"+runID)
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return transcodeOutput{}, fmt.Errorf("create temp dir: %w", err)
	}

	tempPath := filepath.Join(tempDir, filepath.Base(input.FilePath))
	if err := ffmpeg.NewTranscode(input.FilePath, tempPath).
		ToVideoCodec(videoCodec).
		ToContainer(ffmpeg.ContainerMKV).
		Build().
		Run(ctx); err != nil {
		if removeErr := os.RemoveAll(tempDir); removeErr != nil {
			return transcodeOutput{}, errors.Join(
				fmt.Errorf("transcode: %w", err),
				fmt.Errorf("cleanup temp dir: %w", removeErr),
			)
		}
		return transcodeOutput{}, fmt.Errorf("transcode: %w", err)
	}

	return transcodeOutput{TempPath: tempPath}, nil
}
