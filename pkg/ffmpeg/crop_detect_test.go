package ffmpeg_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

// TestDetectCrop_BlackBars verifies that a video padded with 20px black bars on
// the top and bottom is detected as a cropped region smaller than the padded
// dimensions.
//
// The fixture is 320x220 (original 320x180 + 20px bars top/bottom). Cropdetect
// with its default round=16 reports h=176 (nearest multiple of 16 below 180)
// and y=22 (bar height + centering adjustment).
func TestDetectCrop_BlackBars(t *testing.T) {
	params, err := ffmpeg.DetectCrop(t.Context(), "testdata/video_black_bars.mp4")
	require.NoError(t, err)

	assert.Equal(t, 320, params.W, "crop width should match content width")
	assert.Equal(t, 176, params.H, "crop height should exclude black bars (round=16 reduces 180→176)")
	assert.Equal(t, 0, params.X, "crop x should be 0 (no horizontal bars)")
	assert.Equal(t, 22, params.Y, "crop y should reflect bar height plus centering adjustment")
}

// TestDetectCrop_NoBars verifies that a video with no black bars returns the
// full input dimensions with x=0, y=0. The fixture is a 320x160 solid-color
// video; 160 is a multiple of 16 so cropdetect's round=16 does not trim it.
func TestDetectCrop_NoBars(t *testing.T) {
	params, err := ffmpeg.DetectCrop(t.Context(), "testdata/video_no_bars.mp4")
	require.NoError(t, err)

	assert.Equal(t, 320, params.W, "crop width should be full width")
	assert.Equal(t, 160, params.H, "crop height should be full height")
	assert.Equal(t, 0, params.X, "crop x should be 0")
	assert.Equal(t, 0, params.Y, "crop y should be 0")
}

// TestDetectCrop_ContextCancelled verifies that a pre-cancelled context causes
// DetectCrop to return promptly with the context's error.
func TestDetectCrop_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ffmpeg.DetectCrop(ctx, "testdata/video_black_bars.mp4")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
