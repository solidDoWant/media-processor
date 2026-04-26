package steps

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// testVideoPath points to the small H.264/MP4 clip shared with the ffprobe package.
const testVideoPath = "../../pkg/ffprobe/testdata/video.mp4"

// testTwoAudioVideoPath points to a variant of the test clip that has two AAC
// audio streams: stream 1 tagged "jpn" and stream 2 tagged "und".
const testTwoAudioVideoPath = "testdata/video_two_audio.mp4"

// copyTestVideo copies the shared test video to a temp file and returns its path.
// The file is NOT registered for cleanup so tests can verify deletion behaviour.
func copyTestVideo(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile(testVideoPath)
	require.NoError(t, err)

	dst := filepath.Join(t.TempDir(), "video.mp4")
	require.NoError(t, os.WriteFile(dst, src, 0o600))

	return dst
}

// copyTwoAudioTestVideo copies the two-audio-stream test video to a temp file
// and returns its path. The file is NOT registered for cleanup so tests can
// verify deletion behaviour.
func copyTwoAudioTestVideo(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile(testTwoAudioVideoPath)
	require.NoError(t, err)

	dst := filepath.Join(t.TempDir(), "video_two_audio.mp4")
	require.NoError(t, os.WriteFile(dst, src, 0o600))

	return dst
}
