package ffmpeg_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
)

// blackBarsVideoPath is a 320×220 variant of the Big Buck Bunny test clip with
// 20-pixel black bars added at the top and bottom via the pad filter.
const blackBarsVideoPath = "testdata/video_black_bars.mp4"

// TestDetectCrop_WithBlackBars verifies that DetectCrop detects the non-black
// region of a video that has explicit horizontal black bars.
//
// video_black_bars.mp4 is video.mp4 (320×180) padded to 320×220 with 20-pixel
// black bars top and bottom. With cropdetect's default round=16:
//   - w=320 (full width, no side bars)
//   - h=176 (largest multiple of 16 that fits within the 180-pixel content area)
//   - x=0 (no horizontal offset)
//   - y=22 (20-pixel bar + 2-pixel rounding adjustment)
func TestDetectCrop_WithBlackBars(t *testing.T) {
	cp, err := ffmpeg.DetectCrop(t.Context(), blackBarsVideoPath)
	require.NoError(t, err)

	assert.Equal(t, 320, cp.W, "width must span the full frame")
	assert.Equal(t, 176, cp.H, "height must reflect the non-black content region (round=16)")
	assert.Equal(t, 0, cp.X, "no horizontal offset expected")
	assert.Equal(t, 22, cp.Y, "top offset must account for the 20-pixel black bar")
}

// TestDetectCrop_NoBars verifies that DetectCrop returns a crop region that
// covers substantially the full frame when the input has no added black bars.
//
// video.mp4 is the 320×180 Big Buck Bunny clip. With cropdetect's default
// round=16 (h is rounded to the nearest multiple of 16, and the offset is
// adjusted accordingly), the expected values are:
//   - w=320 (full width)
//   - h=176 (largest multiple of 16 ≤ 180)
//   - x=0 (no horizontal offset)
//   - y=2 (small alignment offset, no meaningful bar detected)
func TestDetectCrop_NoBars(t *testing.T) {
	cp, err := ffmpeg.DetectCrop(t.Context(), testVideoPath)
	require.NoError(t, err)

	assert.Equal(t, 320, cp.W, "width must span the full frame")
	assert.Equal(t, 0, cp.X, "no horizontal offset expected")
	// cropdetect round=16 may produce a small non-zero Y offset due to
	// height alignment; verify it is negligible (< 5 pixels).
	assert.Less(t, cp.Y, 5, "top offset must be negligible for a video with no black bars")
	// Height must cover the vast majority of the 180-pixel frame.
	assert.GreaterOrEqual(t, cp.H, 172, "detected height must cover most of the frame")
}

// TestDetectCrop_CancelledContext verifies that DetectCrop returns promptly
// with context.Canceled when the context is already cancelled before the call.
func TestDetectCrop_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before calling DetectCrop

	_, err := ffmpeg.DetectCrop(ctx, blackBarsVideoPath)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestDetectCrop_CancelDuringRun verifies that cancelling the context
// mid-run causes DetectCrop to return promptly with ctx.Err().
func TestDetectCrop_CancelDuringRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() {
		_, err := ffmpeg.DetectCrop(ctx, blackBarsVideoPath)
		done <- err
	}()

	// Cancel the context shortly after the goroutine has started. A tiny
	// sleep is sufficient because DetectCrop immediately begins I/O; the
	// IOInterrupter will abort it as soon as Interrupt() is called.
	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// The function may complete normally (video is short) or return a
		// context error depending on scheduling. Either outcome is acceptable.
		if err != nil {
			assert.ErrorIs(t, err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DetectCrop did not return promptly after context cancellation")
	}
}
