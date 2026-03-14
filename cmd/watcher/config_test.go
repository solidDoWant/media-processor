package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadConfig covers AC3 (valid config loads without error) and
// AC4 (invalid/missing config causes a descriptive error).
func TestLoadConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   string     // empty string → missing file test
		wantErr   bool
		wantLen   int
		wantFirst WatchEntry
	}{
		{
			name: "valid config with two entries",
			content: `
watches:
  - path: /watch/movies
    workflow: MovieWorkflow
  - path: /watch/shows
    workflow: ShowWorkflow
`,
			wantLen:   2,
			wantFirst: WatchEntry{Path: "/watch/movies", Workflow: "MovieWorkflow"},
		},
		{
			name:    "invalid YAML",
			content: "{ this is: [not valid yaml",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeTempConfig(t, tt.content)

			cfg, err := loadConfig(path)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, cfg.Watches, tt.wantLen)
			if tt.wantLen > 0 {
				assert.Equal(t, tt.wantFirst, cfg.Watches[0])
			}
		})
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	t.Parallel()
	// AC4: missing file path → descriptive error.
	_, err := loadConfig("/nonexistent/path/config.yaml")
	require.Error(t, err)
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
