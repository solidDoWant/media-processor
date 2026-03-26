package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/internal/watcherconfig"
)

// TestValidateWatchDirs verifies that validateWatchDirs returns a descriptive error
// when a configured watch directory does not exist, and succeeds when all dirs are present.
func TestValidateWatchDirs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *Config
		errFunc require.ErrorAssertionFunc
	}{
		{
			name: "existing directory passes",
			cfg: &Config{
				Watches: []WatchEntry{
					{Path: t.TempDir(), MediaType: "M"},
				},
			},
			errFunc: require.NoError,
		},
		{
			name: "missing directory returns error",
			cfg: &Config{
				Watches: []WatchEntry{
					{Path: "/nonexistent/path/abc123", MediaType: "M"},
				},
			},
			errFunc: require.Error,
		},
		{
			name: "all errors reported when multiple dirs are missing",
			cfg: &Config{
				Watches: []WatchEntry{
					{Path: "/nonexistent/alpha", MediaType: "M"},
					{Path: "/nonexistent/beta", MediaType: "M"},
				},
			},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.Error(t, err, msgAndArgs...)
				assert.Contains(t, err.Error(), "/nonexistent/alpha")
				assert.Contains(t, err.Error(), "/nonexistent/beta")
			},
		},
		{
			name:    "empty watch list passes",
			cfg:     &Config{},
			errFunc: require.NoError,
		},
		{
			name: "path that exists but is a file returns error",
			cfg: func() *Config {
				f, err := os.CreateTemp(t.TempDir(), "notadir")
				require.NoError(t, err)
				require.NoError(t, f.Close())
				return &Config{Watches: []WatchEntry{{Path: f.Name(), MediaType: "M"}}}
			}(),
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.Error(t, err, msgAndArgs...)
				assert.Contains(t, err.Error(), "not a directory")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.errFunc(t, validateWatchDirs(tt.cfg))
		})
	}
}

// TestScan_FileInWatchedDir verifies that a file present in a configured watch directory
// is dispatched with the correct media type and absolute file path.
func TestScan_FileInWatchedDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "movie.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: dir, MediaType: watcherconfig.Movie},
		},
	}

	type call struct {
		filePath  string
		mediaType watcherconfig.MediaType
	}
	var calls []call
	dispatch := func(_ context.Context, fp string, mt watcherconfig.MediaType) error {
		calls = append(calls, call{fp, mt})
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, dispatch))
	require.Len(t, calls, 1)
	assert.Equal(t, filePath, calls[0].filePath)
	assert.Equal(t, watcherconfig.Movie, calls[0].mediaType)
}

// TestScan_SubdirectoryFilesUseParentMapping verifies that files within subdirectories
// of a configured watch path are dispatched using the parent watch entry's media type.
func TestScan_SubdirectoryFilesUseParentMapping(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subdir := filepath.Join(dir, "show-title")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	filePath := filepath.Join(subdir, "episode.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: dir, MediaType: watcherconfig.Show},
		},
	}

	var dispatched []string
	dispatch := func(_ context.Context, fp string, _ watcherconfig.MediaType) error {
		dispatched = append(dispatched, fp)
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, dispatch))
	require.Len(t, dispatched, 1)
	assert.Equal(t, filePath, dispatched[0])
}

// TestScan_DispatchErrorsAreAggregated verifies that dispatch errors do not abort the
// scan — all files are still processed — and the aggregate error is returned.
func TestScan_DispatchErrorsAreAggregated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: dir, MediaType: "M"},
		},
	}

	var count int
	dispatch := func(_ context.Context, _ string, _ watcherconfig.MediaType) error {
		count++
		return errors.New("simulated dispatch failure")
	}

	require.Error(t, scan(t.Context(), cfg, dispatch))
	assert.Equal(t, 2, count)
}

// TestScan_ContextCancellationStopsWalk verifies that cancelling the context causes scan
// to stop walking and return the context error rather than an aggregate scan error.
func TestScan_ContextCancellationStopsWalk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: dir, MediaType: "M"},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately before scan starts

	err := scan(ctx, cfg, func(_ context.Context, _ string, _ watcherconfig.MediaType) error { return nil })
	assert.ErrorIs(t, err, context.Canceled)
}

// TestScan_MultipleWatchEntries verifies that files in separate watch directories are
// each dispatched with their respective configured media types.
func TestScan_MultipleWatchEntries(t *testing.T) {
	t.Parallel()

	movieDir := t.TempDir()
	showDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(movieDir, "movie.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(showDir, "show.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: movieDir, MediaType: watcherconfig.Movie},
			{Path: showDir, MediaType: watcherconfig.Show},
		},
	}

	dispatched := make(map[string]watcherconfig.MediaType) // path → media type
	dispatch := func(_ context.Context, fp string, mt watcherconfig.MediaType) error {
		dispatched[fp] = mt
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, dispatch))

	assert.Equal(t, watcherconfig.Movie, dispatched[filepath.Join(movieDir, "movie.mkv")])
	assert.Equal(t, watcherconfig.Show, dispatched[filepath.Join(showDir, "show.mkv")])
}
