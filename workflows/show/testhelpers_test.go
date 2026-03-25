package show

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
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

// stubEpisodeLibrary implements medialib.EpisodeLibrary for testing.
type stubEpisodeLibrary struct {
	episode      medialib.Episode
	err          error
	refreshCalls []int64
}

func (s *stubEpisodeLibrary) GetEpisodeByFilePath(_ context.Context, _ string) (medialib.Episode, error) {
	return s.episode, s.err
}

func (s *stubEpisodeLibrary) RefreshSeries(_ context.Context, seriesID int64) error {
	s.refreshCalls = append(s.refreshCalls, seriesID)
	return s.err
}
