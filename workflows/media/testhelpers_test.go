package media

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

// stubLibraryClient implements medialib.ArrLibrary for testing.
type stubLibraryClient struct {
	err         error
	importCalls []string
	infoResult  medialib.MediaInfo
	infoErr     error
	posterBytes []byte
	posterMime  string
	posterErr   error
}

func (s *stubLibraryClient) ImportByFilePath(_ context.Context, path string) error {
	s.importCalls = append(s.importCalls, path)
	return s.err
}

func (s *stubLibraryClient) GetInfo(_ context.Context, _ string) (medialib.MediaInfo, error) {
	return s.infoResult, s.infoErr
}

func (s *stubLibraryClient) GetPosterImage(_ context.Context, _ string) ([]byte, string, error) {
	return s.posterBytes, s.posterMime, s.posterErr
}
