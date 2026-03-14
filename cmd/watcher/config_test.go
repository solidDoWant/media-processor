package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadConfig verifies that the watcher correctly parses its YAML config file,
// loading directory-to-workflow mappings for valid input and returning a descriptive
// error for invalid or missing files.
func TestLoadConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  string
		errFunc  require.ErrorAssertionFunc // defaults to require.NoError
		expected Config
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
			expected: Config{
				Watches: []WatchEntry{
					{Path: "/watch/movies", Workflow: "MovieWorkflow"},
					{Path: "/watch/shows", Workflow: "ShowWorkflow"},
				},
			},
		},
		{
			name:    "invalid YAML returns error",
			content: "{ this is: [not valid yaml",
			errFunc: require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			errFunc := tt.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			path := writeTempConfig(t, tt.content)
			cfg, err := loadConfig(path)
			errFunc(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, tt.expected, *cfg)
		})
	}
}

// TestLoadConfig_MissingFile verifies that loadConfig returns a descriptive error
// when the specified config file does not exist.
func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := loadConfig("/nonexistent/path/config.yaml")
	require.Error(t, err)
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
