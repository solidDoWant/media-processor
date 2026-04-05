package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

func TestApplyCropThresholds(t *testing.T) {
	// Input video dimensions used across all cases.
	const (
		inputW = 1920
		inputH = 1080
	)

	tests := []struct {
		name     string
		detected ffmpeg.CropParams
		minCropX int
		minCropY int
		wantNil  bool
		wantCrop ffmpeg.CropParams
	}{
		{
			name:     "crop exceeds both thresholds — full crop applied",
			detected: ffmpeg.CropParams{W: 1880, H: 1040, X: 20, Y: 20},
			minCropX: 10,
			minCropY: 10,
			wantNil:  false,
			wantCrop: ffmpeg.CropParams{W: 1880, H: 1040, X: 20, Y: 20},
		},
		{
			name:     "horizontal trim below threshold — x axis zeroed, y kept",
			detected: ffmpeg.CropParams{W: 1915, H: 1040, X: 2, Y: 20},
			minCropX: 10,
			minCropY: 10,
			wantNil:  false,
			wantCrop: ffmpeg.CropParams{W: inputW, H: 1040, X: 0, Y: 20},
		},
		{
			name:     "vertical trim below threshold — y axis zeroed, x kept",
			detected: ffmpeg.CropParams{W: 1880, H: 1075, X: 20, Y: 2},
			minCropX: 10,
			minCropY: 10,
			wantNil:  false,
			wantCrop: ffmpeg.CropParams{W: 1880, H: inputH, X: 20, Y: 0},
		},
		{
			name:     "both axes below threshold — nil returned",
			detected: ffmpeg.CropParams{W: 1918, H: 1078, X: 1, Y: 1},
			minCropX: 10,
			minCropY: 10,
			wantNil:  true,
		},
		{
			name:     "minCropX disabled (-1) — x always accepted",
			detected: ffmpeg.CropParams{W: 1918, H: 1040, X: 1, Y: 20},
			minCropX: -1,
			minCropY: 10,
			wantNil:  false,
			wantCrop: ffmpeg.CropParams{W: 1918, H: 1040, X: 1, Y: 20},
		},
		{
			name:     "minCropY disabled (-1) — y always accepted",
			detected: ffmpeg.CropParams{W: 1880, H: 1078, X: 20, Y: 1},
			minCropX: 10,
			minCropY: -1,
			wantNil:  false,
			wantCrop: ffmpeg.CropParams{W: 1880, H: 1078, X: 20, Y: 1},
		},
		{
			name:     "detected equals input dimensions — nil returned (no crop)",
			detected: ffmpeg.CropParams{W: inputW, H: inputH, X: 0, Y: 0},
			minCropX: 10,
			minCropY: 10,
			wantNil:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyCropThresholds(tc.detected, inputW, inputH, tc.minCropX, tc.minCropY)
			if tc.wantNil {
				assert.Nil(t, got)
			} else {
				if assert.NotNil(t, got) {
					assert.Equal(t, tc.wantCrop, *got)
				}
			}
		})
	}
}

func TestRunDetectCrop_BothDisabled(t *testing.T) {
	// When both minCropX and minCropY are -1, RunDetectCrop must return nil
	// without calling ffmpeg.DetectCrop (no video file needed).
	got, err := RunDetectCrop(t.Context(), "/nonexistent/file.mkv", 1920, 1080, -1, -1)
	assert.NoError(t, err)
	assert.Nil(t, got, "expected nil when both axes are disabled")
}
