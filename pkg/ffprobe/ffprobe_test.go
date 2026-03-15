package ffprobe_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

func TestProbe_ValidFile(t *testing.T) {
	info, err := ffprobe.Probe(t.Context(), filepath.Join("testdata", "video.mp4"))
	require.NoError(t, err)
	require.NotNil(t, info)

	// Container-level assertions against known values for testdata/video.mp4
	// (first ~5s of Big Buck Bunny, re-encoded at 320x180).
	assert.Equal(t, "mov,mp4,m4a,3gp,3g2,mj2", info.Format)
	assert.Equal(t, 5013333*time.Microsecond, info.Duration)
	assert.Equal(t, int64(607664), info.BitRateBitsPerSecond)

	// Container tags.
	assert.Equal(t, "Big Buck Bunny", info.Tags["title"])
	assert.Equal(t, "Blender Foundation", info.Tags["artist"])

	// Two streams: video (index 0) and audio (index 1).
	require.Len(t, info.Streams, 2)

	video := info.Streams[0]
	assert.Equal(t, "h264", video.CodecName)
	assert.Equal(t, ffprobe.CodecTypeVideo, video.CodecType)
	assert.Equal(t, int64(441324), video.BitRateBitsPerSecond)
	assert.Equal(t, 320, video.WidthPixels)
	assert.Equal(t, 180, video.HeightPixels)
	assert.Equal(t, 24.0, video.FramesPerSecond)
	assert.Zero(t, video.AudioSampleRateHz)
	assert.Zero(t, video.AudioChannelCount)

	audio := info.Streams[1]
	assert.Equal(t, "aac", audio.CodecName)
	assert.Equal(t, ffprobe.CodecTypeAudio, audio.CodecType)
	assert.Equal(t, int64(161052), audio.BitRateBitsPerSecond)
	assert.Zero(t, audio.WidthPixels)
	assert.Zero(t, audio.HeightPixels)
	assert.Zero(t, audio.FramesPerSecond)
	assert.Equal(t, 48000, audio.AudioSampleRateHz)
	assert.Equal(t, 2, audio.AudioChannelCount)
}

func TestProbe_NonExistentFile(t *testing.T) {
	_, err := ffprobe.Probe(t.Context(), "/no/such/file.mp4")
	assert.Error(t, err)
}

func TestProbe_NonMediaFile(t *testing.T) {
	// Use this test file itself — a valid file but not a media container.
	_, err := ffprobe.Probe(t.Context(), "ffprobe_test.go")
	assert.Error(t, err)
}

func TestProbe_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before calling Probe

	_, err := ffprobe.Probe(ctx, filepath.Join("testdata", "video.mp4"))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
