package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
					{Path: t.TempDir(), Workflow: "W"},
				},
			},
			errFunc: require.NoError,
		},
		{
			name: "missing directory returns error",
			cfg: &Config{
				Watches: []WatchEntry{
					{Path: "/nonexistent/path/abc123", Workflow: "W"},
				},
			},
			errFunc: require.Error,
		},
		{
			name: "all errors reported when multiple dirs are missing",
			cfg: &Config{
				Watches: []WatchEntry{
					{Path: "/nonexistent/alpha", Workflow: "W"},
					{Path: "/nonexistent/beta", Workflow: "W"},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.errFunc(t, validateWatchDirs(tt.cfg))
		})
	}
}

// TestScan_FileInWatchedDir verifies that a file present in a configured watch directory
// is dispatched with the correct workflow name and absolute file path.
func TestScan_FileInWatchedDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "movie.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: dir, Workflow: "MovieWorkflow"},
		},
	}

	type call struct{ workflow, path string }
	var calls []call
	dispatch := func(_ context.Context, workflow, path string) error {
		calls = append(calls, call{workflow, path})
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, dispatch))
	require.Len(t, calls, 1)
	assert.Equal(t, "MovieWorkflow", calls[0].workflow)
	assert.Equal(t, filePath, calls[0].path)
}

// TestScan_SubdirectoryFilesUseParentMapping verifies that files within subdirectories
// of a configured watch path are dispatched using the parent watch entry's workflow.
func TestScan_SubdirectoryFilesUseParentMapping(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subdir := filepath.Join(dir, "show-title")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	filePath := filepath.Join(subdir, "episode.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: dir, Workflow: "ShowWorkflow"},
		},
	}

	var dispatched []string
	dispatch := func(_ context.Context, _, path string) error {
		dispatched = append(dispatched, path)
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
			{Path: dir, Workflow: "W"},
		},
	}

	var count int
	dispatch := func(_ context.Context, _, _ string) error {
		count++
		return errors.New("simulated dispatch failure")
	}

	require.Error(t, scan(t.Context(), cfg, dispatch))
	assert.Equal(t, 2, count)
}

// TestScan_MultipleWatchEntries verifies that files in separate watch directories are
// each dispatched with their respective configured workflow names.
func TestScan_MultipleWatchEntries(t *testing.T) {
	t.Parallel()

	movieDir := t.TempDir()
	showDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(movieDir, "movie.mkv"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(showDir, "show.mkv"), []byte{}, 0o600))

	cfg := &Config{
		Watches: []WatchEntry{
			{Path: movieDir, Workflow: "MovieWorkflow"},
			{Path: showDir, Workflow: "ShowWorkflow"},
		},
	}

	dispatched := make(map[string]string) // path → workflow
	dispatch := func(_ context.Context, workflow, path string) error {
		dispatched[path] = workflow
		return nil
	}

	require.NoError(t, scan(t.Context(), cfg, dispatch))
	assert.Equal(t, "MovieWorkflow", dispatched[filepath.Join(movieDir, "movie.mkv")])
	assert.Equal(t, "ShowWorkflow", dispatched[filepath.Join(showDir, "show.mkv")])
}
