package media

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteSentinel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		fileName     string
		wantSentinel string
	}{
		{
			name:         "standard file name",
			fileName:     "movie.mkv",
			wantSentinel: ".movie.mkv.done",
		},
		{
			name:         "file without extension",
			fileName:     "movie",
			wantSentinel: ".movie.done",
		},
		{
			name:         "file with multiple dots",
			fileName:     "movie.2024.mkv",
			wantSentinel: ".movie.2024.mkv.done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			filePath := filepath.Join(dir, tt.fileName)
			expectedSentinel := filepath.Join(dir, tt.wantSentinel)

			require.NoError(t, WriteSentinel(filePath))

			_, statErr := os.Stat(expectedSentinel)
			assert.NoError(t, statErr, "sentinel file should exist at %s", expectedSentinel)
		})
	}
}

func TestWriteSentinel_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "movie.mkv")

	require.NoError(t, WriteSentinel(filePath))
	require.NoError(t, WriteSentinel(filePath), "second call should not error")

	_, statErr := os.Stat(filepath.Join(dir, ".movie.mkv.done"))
	assert.NoError(t, statErr, "sentinel should still exist after second write")
}

func TestRunCleanup(t *testing.T) {
	tests := []struct {
		name        string
		setupPath   func(t *testing.T) string
		errFunc     require.ErrorAssertionFunc
		fileDeleted bool
	}{
		{
			name: "existing file is deleted",
			setupPath: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "source.mkv")
				require.NoError(t, os.WriteFile(p, []byte("data"), 0o600))

				return p
			},
			errFunc:     require.NoError,
			fileDeleted: true,
		},
		{
			name: "non-existent path returns no error",
			setupPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist.mkv")
			},
			errFunc:     require.NoError,
			fileDeleted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setupPath(t)

			err := RunCleanup(path, "", false)

			tt.errFunc(t, err)

			if tt.fileDeleted {
				_, statErr := os.Stat(path)
				assert.True(t, os.IsNotExist(statErr), "expected file to be deleted")
			}
		})
	}
}

// TestRunCleanup_PrunesEmptyParents verifies that empty parent directories between the
// deleted file and the watch root are removed bottom-up after cleanup.
func TestRunCleanup_PrunesEmptyParents(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	subdir := filepath.Join(watchRoot, "Some.Release.Name")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	filePath := filepath.Join(subdir, "video.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte("data"), 0o600))

	require.NoError(t, RunCleanup(filePath, watchRoot, false))

	_, statErr := os.Stat(filePath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted")

	_, statErr = os.Stat(subdir)
	assert.True(t, os.IsNotExist(statErr), "empty wrapper directory should be removed")

	_, statErr = os.Stat(watchRoot)
	assert.NoError(t, statErr, "watch root must not be removed")
}

// TestRunCleanup_StopsAtNonEmptyParent verifies that traversal stops when a sibling file
// exists next to the deleted source file.
func TestRunCleanup_StopsAtNonEmptyParent(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	subdir := filepath.Join(watchRoot, "release")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	filePath := filepath.Join(subdir, "video.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte("data"), 0o600))

	sibling := filepath.Join(subdir, "subtitle.srt")
	require.NoError(t, os.WriteFile(sibling, []byte("sub"), 0o600))

	require.NoError(t, RunCleanup(filePath, watchRoot, false))

	_, statErr := os.Stat(subdir)
	assert.NoError(t, statErr, "directory with sibling file should not be removed")
}

// TestRunCleanup_RetainEmptyDirsSkipsPruning verifies that empty parent directories are
// left intact when retainEmptyDirs is true.
func TestRunCleanup_RetainEmptyDirsSkipsPruning(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	subdir := filepath.Join(watchRoot, "release")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	filePath := filepath.Join(subdir, "video.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte("data"), 0o600))

	require.NoError(t, RunCleanup(filePath, watchRoot, true))

	_, statErr := os.Stat(filePath)
	assert.True(t, os.IsNotExist(statErr), "source file should still be deleted")

	_, statErr = os.Stat(subdir)
	assert.NoError(t, statErr, "empty parent should be kept when retainEmptyDirs is true")
}

// TestRunCleanup_WatchRootNotRemoved verifies that the watch root itself is never removed
// even when it becomes empty after the source file is deleted.
func TestRunCleanup_WatchRootNotRemoved(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	filePath := filepath.Join(watchRoot, "video.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte("data"), 0o600))

	require.NoError(t, RunCleanup(filePath, watchRoot, false))

	_, statErr := os.Stat(watchRoot)
	assert.NoError(t, statErr, "watch root must not be removed even when empty")
}

// TestRunCleanup_PrunesMultiLevelEmptyChain verifies that multiple levels of empty
// ancestor directories are removed bottom-up until a non-empty ancestor or the watch root.
func TestRunCleanup_PrunesMultiLevelEmptyChain(t *testing.T) {
	t.Parallel()

	watchRoot := t.TempDir()
	nested := filepath.Join(watchRoot, "a", "b", "c")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	filePath := filepath.Join(nested, "video.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte("data"), 0o600))

	require.NoError(t, RunCleanup(filePath, watchRoot, false))

	_, statErr := os.Stat(filePath)
	assert.True(t, os.IsNotExist(statErr), "source file should be deleted")

	for _, dir := range []string{
		filepath.Join(watchRoot, "a", "b", "c"),
		filepath.Join(watchRoot, "a", "b"),
		filepath.Join(watchRoot, "a"),
	} {
		_, statErr = os.Stat(dir)
		assert.True(t, os.IsNotExist(statErr), "empty ancestor %q should be removed", dir)
	}

	_, statErr = os.Stat(watchRoot)
	assert.NoError(t, statErr, "watch root must not be removed")
}
