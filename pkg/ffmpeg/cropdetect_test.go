package ffmpeg_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

const blackBarsVideoPath = "testdata/video_black_bars.mp4"

// TestDetectCrop_WithBlackBars verifies that a video padded with 20px black bars
// top and bottom reports a smaller crop region. The fixture is 320x220; cropdetect
// with round=16 returns h=176 and y=22.
func TestDetectCrop_WithBlackBars(t *testing.T) {
	params, err := ffmpeg.DetectCrop(t.Context(), blackBarsVideoPath)
	require.NoError(t, err)

	assert.Equal(t, 320, params.W, "crop width should match content width")
	assert.Equal(t, 176, params.H, "crop height should exclude black bars (round=16 reduces 180→176)")
	assert.Equal(t, 0, params.X, "crop x should be 0 (no horizontal bars)")
	assert.Equal(t, 22, params.Y, "crop y should reflect bar height plus centering adjustment")
}

// TestDetectCrop_NoBars verifies that DetectCrop still returns a valid crop
// region for a video with no black bars. The fixture is the standard 320x180
// BBB clip; with round=16, cropdetect reports h≥172 and a small y offset.
func TestDetectCrop_NoBars(t *testing.T) {
	params, err := ffmpeg.DetectCrop(t.Context(), testVideoPath)
	require.NoError(t, err)

	assert.Equal(t, 320, params.W, "crop width should be full content width")
	assert.GreaterOrEqual(t, params.H, 172, "crop height should be close to full height")
	assert.Equal(t, 0, params.X, "crop x should be 0")
	assert.Less(t, params.Y, 5, "crop y should be a small rounding offset")
}

// TestDetectCrop_ContextCancelledBeforeStart verifies that a pre-cancelled
// context causes DetectCrop to return immediately with context.Canceled.
func TestDetectCrop_ContextCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ffmpeg.DetectCrop(ctx, blackBarsVideoPath)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestDetectCrop_ContextCancelledDuringRun verifies that cancelling the context
// mid-run causes DetectCrop to return promptly with context.Canceled.
func TestDetectCrop_ContextCancelledDuringRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := ffmpeg.DetectCrop(ctx, blackBarsVideoPath)

	// The function must complete well before the test timeout.
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
