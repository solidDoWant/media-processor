package ffprobe_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

// testdataPath returns the absolute path to a file under pkg/ffprobe/testdata/.
func testdataPath(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", name)
}

func TestProbe_ValidFile(t *testing.T) {
	info, err := ffprobe.Probe(context.Background(), testdataPath("video.mp4"))
	require.NoError(t, err)

	// Container format must be non-empty.
	assert.NotEmpty(t, info.Format)

	// Duration must be positive.
	assert.Positive(t, info.Duration)

	// Overall bitrate must be positive.
	assert.Positive(t, info.BitRate)

	// Must have at least one stream.
	require.NotEmpty(t, info.Streams)

	// Every stream must have a codec name and type.
	for _, s := range info.Streams {
		assert.NotEmpty(t, s.CodecName)
		assert.NotEmpty(t, s.CodecType)
	}

	// Must have at least one video stream with width, height, and frame rate.
	var videoStreams []ffprobe.StreamInfo
	for _, s := range info.Streams {
		if s.CodecType == "video" {
			videoStreams = append(videoStreams, s)
		}
	}
	require.NotEmpty(t, videoStreams, "expected at least one video stream")
	for _, vs := range videoStreams {
		assert.Positive(t, vs.Width)
		assert.Positive(t, vs.Height)
		assert.Positive(t, vs.FrameRate)
	}
}

func TestProbe_NonExistentFile(t *testing.T) {
	_, err := ffprobe.Probe(context.Background(), "/no/such/file.mp4")
	assert.Error(t, err)
}

func TestProbe_NonMediaFile(t *testing.T) {
	// Use this test file itself — a valid file but not a media container.
	_, thisFile, _, _ := runtime.Caller(0)
	_, err := ffprobe.Probe(context.Background(), thisFile)
	assert.Error(t, err)
}

func TestProbe_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Probe

	_, err := ffprobe.Probe(ctx, testdataPath("video.mp4"))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
