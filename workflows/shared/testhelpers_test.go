package shared

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// testVideoPath points to the small H.264/MP4 clip shared with the ffprobe package.
const testVideoPath = "../../pkg/ffprobe/testdata/video.mp4"

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
