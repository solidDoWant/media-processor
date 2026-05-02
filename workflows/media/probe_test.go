package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunProbe(t *testing.T) {
	tests := []struct {
		name        string
		setupPath   func(t *testing.T) string
		expected    ProbeOutput
		errFunc     require.ErrorAssertionFunc
		fileDeleted bool
	}{
		{
			name: "non-media text file returns invalid and deletes file",
			setupPath: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "notavideo.txt")
				require.NoError(t, os.WriteFile(p, []byte("hello"), 0o600))

				return p
			},
			expected:    ProbeOutput{IsValidMedia: false},
			errFunc:     require.NoError,
			fileDeleted: true,
		},
		{
			name: "non-existent path returns invalid without error",
			setupPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist.mp4")
			},
			expected:    ProbeOutput{IsValidMedia: false},
			errFunc:     require.NoError,
			fileDeleted: false, // nothing to delete
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setupPath(t)

			got, err := RunProbe(t.Context(), path, "", false)

			tt.errFunc(t, err)
			assert.Equal(t, tt.expected, got)

			if tt.fileDeleted {
				_, statErr := os.Stat(path)
				assert.True(t, os.IsNotExist(statErr), "expected file to be deleted")
			}
		})
	}
}

func TestRunProbe_ValidMediaFile(t *testing.T) {
	path := copyTestVideo(t)

	got, err := RunProbe(t.Context(), path, "", false)

	require.NoError(t, err)
	// Check all non-float fields via struct equality (DurationSeconds zeroed out).
	gotWithoutDuration := got
	gotWithoutDuration.DurationSeconds = 0
	assert.Equal(t, ProbeOutput{
		IsValidMedia: true,
		VideoCodec:   "h264",
		Format:       "mov,mp4,m4a,3gp,3g2,mj2",
		AudioStreams: []AudioStreamInfo{
			{StreamInfo: StreamInfo{Index: 1, Language: "und"}, ReportedChannelCount: 2, EffectiveChannelCount: 2},
		},
		VideoWidth:  320,
		VideoHeight: 180,
	}, gotWithoutDuration)
	// DurationSeconds is a float64 derived from ffprobe; use InDelta to tolerate minor rounding.
	assert.InDelta(t, 5.013333, got.DurationSeconds, 0.001, "DurationSeconds should match actual file duration")
	assert.Greater(t, got.DurationSeconds, float64(0), "DurationSeconds should be positive for valid media")
}

func TestRunProbe_CancelledContextPropagatesError(t *testing.T) {
	// A cancelled context must not silently delete the file — it must propagate
	// the context error so the step fails and OnFailure fires.
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	path := copyTestVideo(t)

	_, err := RunProbe(ctx, path, "", false)
	require.ErrorIs(t, err, context.Canceled, "cancelled context should propagate as error, not delete the file")

	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "file must not be deleted on context cancellation")
}

// TestRunProbe_PrunesEmptyParentsOnInvalidFile verifies that when a non-media file is
// deleted by the probe step, empty parent directories up to the watch root are removed.
func TestRunProbe_PrunesEmptyParentsOnInvalidFile(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	subdir := filepath.Join(watchRoot, "Some.Release.Name")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	filePath := filepath.Join(subdir, "notavideo.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0o600))

	got, err := RunProbe(t.Context(), filePath, watchRoot, false)
	require.NoError(t, err)
	assert.False(t, got.IsValidMedia)

	_, statErr := os.Stat(filePath)
	assert.True(t, os.IsNotExist(statErr), "invalid file should be deleted")

	_, statErr = os.Stat(subdir)
	assert.True(t, os.IsNotExist(statErr), "empty wrapper directory should be removed")

	_, statErr = os.Stat(watchRoot)
	assert.NoError(t, statErr, "watch root must not be removed")
}

// TestRunProbe_StopsAtNonEmptyParentOnInvalidFile verifies that traversal stops when a
// sibling file exists in the same directory as the deleted file.
func TestRunProbe_StopsAtNonEmptyParentOnInvalidFile(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	subdir := filepath.Join(watchRoot, "release")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	filePath := filepath.Join(subdir, "notavideo.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0o600))

	sibling := filepath.Join(subdir, "other.mkv")
	require.NoError(t, os.WriteFile(sibling, []byte("data"), 0o600))

	got, err := RunProbe(t.Context(), filePath, watchRoot, false)
	require.NoError(t, err)
	assert.False(t, got.IsValidMedia)

	_, statErr := os.Stat(subdir)
	assert.NoError(t, statErr, "directory with sibling file should not be removed")
}

// TestRunProbe_RetainEmptyDirsSkipsPruning verifies that empty parent directories are
// left intact when retainEmptyDirs is true.
func TestRunProbe_RetainEmptyDirsSkipsPruning(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	subdir := filepath.Join(watchRoot, "release")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	filePath := filepath.Join(subdir, "notavideo.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0o600))

	got, err := RunProbe(t.Context(), filePath, watchRoot, true)
	require.NoError(t, err)
	assert.False(t, got.IsValidMedia)

	_, statErr := os.Stat(filePath)
	assert.True(t, os.IsNotExist(statErr), "invalid file should still be deleted")

	_, statErr = os.Stat(subdir)
	assert.NoError(t, statErr, "empty parent should be kept when retainEmptyDirs is true")
}

// TestRunProbe_StillImageIsDeletedAndMarkedInvalid verifies that still image
// files (e.g. PNG) are rejected even though FFmpeg reports them as having a
// video stream. The format name "png_pipe" (and the broader "*_pipe" / "image2"
// family) identifies image-only demuxers and must not be treated as video.
func TestRunProbe_StillImageIsDeletedAndMarkedInvalid(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))

	filePath := filepath.Join(t.TempDir(), "artwork.png")
	require.NoError(t, os.WriteFile(filePath, buf.Bytes(), 0o600))

	got, err := RunProbe(t.Context(), filePath, "", false)
	require.NoError(t, err)
	assert.False(t, got.IsValidMedia, "PNG file must not be treated as valid media")

	_, statErr := os.Stat(filePath)
	assert.True(t, os.IsNotExist(statErr), "PNG file should be deleted by the probe step")
}

func TestIsStillImageFormat(t *testing.T) {
	tests := []struct {
		format   string
		expected bool
	}{
		// Image-pipe demuxers — all must be rejected.
		{"png_pipe", true},
		{"jpeg_pipe", true},
		{"bmp_pipe", true},
		{"webp_pipe", true},
		{"tga_pipe", true},
		{"dpx_pipe", true},
		{"exr_pipe", true},
		// Generic image demuxer.
		{"image2", true},
		// Real video container formats — must not be rejected.
		{"matroska,webm", false},
		{"mov,mp4,m4a,3gp,3g2,mj2", false},
		{"avi", false},
		{"mpegts", false},
		{"asf", false},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			assert.Equal(t, tt.expected, isStillImageFormat(tt.format))
		})
	}
}
