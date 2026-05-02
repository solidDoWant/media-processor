package media

import (
	"context"
	"log/slog"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

// DetectCropOutput is the output of the detectcrop workflow task.
type DetectCropOutput struct {
	// Crop is the detected crop region, or nil when no crop should be applied.
	Crop *ffmpeg.CropParams `json:"crop,omitempty"`
}

// RunDetectCrop runs the ffmpeg cropdetect filter over filePath and returns the
// crop region to apply, or nil when no crop is needed.
//
// minCropX and minCropY are the minimum number of pixels that must be trimmed
// on each horizontal/vertical axis for a crop to be applied. A value of -1
// disables the threshold for that axis (any detected crop is accepted). When
// both are -1 the crop detection step is skipped entirely and ffmpeg.DetectCrop
// is never called.
//
// inputW and inputH are the pixel dimensions of the source video (from the
// ProbeOutput). They are used to compute how many pixels would be trimmed on
// each axis.
func RunDetectCrop(ctx context.Context, filePath string, inputW, inputH, minCropX, minCropY int) (*ffmpeg.CropParams, error) {
	if minCropX == -1 && minCropY == -1 {
		slog.DebugContext(ctx, "crop detection skipped", slog.String("file", filePath), slog.String("reason", "both axes disabled"))
		return nil, nil
	}

	detected, err := ffmpeg.DetectCrop(ctx, filePath)
	if err != nil {
		return nil, err
	}

	result := applyCropThresholds(detected, inputW, inputH, minCropX, minCropY)

	logAttrs := []any{
		slog.String("file", filePath),
		slog.Int("detected_w", detected.W), slog.Int("detected_h", detected.H),
		slog.Int("detected_x", detected.X), slog.Int("detected_y", detected.Y),
		slog.Bool("applied", result != nil),
	}
	if result == nil {
		logAttrs = append(logAttrs, slog.String("skip_reason", "below pixel threshold"))
	}

	slog.DebugContext(ctx, "crop detection complete", logAttrs...)

	return result, nil
}

// applyCropThresholds returns a pointer to detected when the crop exceeds the
// per-axis minimum thresholds, or nil when the crop is too small to be worth
// applying.
//
// For each axis the "trim" is the total number of pixels removed:
//   - Horizontal: inputW - detected.W
//   - Vertical:   inputH - detected.H
//
// A threshold of -1 disables the check for that axis (any crop is accepted).
// When neither axis crosses its threshold, nil is returned (no crop applied).
func applyCropThresholds(detected ffmpeg.CropParams, inputW, inputH, minCropX, minCropY int) *ffmpeg.CropParams {
	trimX := inputW - detected.W
	trimY := inputH - detected.H

	xOK := minCropX == -1 || trimX >= minCropX
	yOK := minCropY == -1 || trimY >= minCropY

	if !xOK && !yOK {
		return nil
	}

	result := detected

	// If only one axis meets its threshold, zero out the other axis so we
	// don't introduce a partial crop that may skew the aspect ratio.
	if !xOK {
		result.W = inputW
		result.X = 0
	}

	if !yOK {
		result.H = inputH
		result.Y = 0
	}

	// After zeroing out a failing axis the result may equal the input
	// dimensions on both axes, meaning nothing is actually cropped.
	if result.W == inputW && result.H == inputH {
		return nil
	}

	return &result
}
