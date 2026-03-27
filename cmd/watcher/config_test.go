package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/internal/watcherconfig"
	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// TestLoadConfig verifies that the watcher correctly parses its YAML config file,
// loading directory-to-media-type mappings for valid input and returning a descriptive
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
    media_type: movie
  - path: /watch/shows
    media_type: show
`,
			expected: Config{
				CronSchedule: watcherconfig.DefaultCronSchedule,
				Watches: []WatchEntry{
					{Path: "/watch/movies", MediaType: medialib.MovieType},
					{Path: "/watch/shows", MediaType: medialib.ShowType},
				},
			},
		},
		{
			name: "custom cron_schedule is parsed",
			content: `
cron_schedule: "*/10 * * * * *"
watches: []
`,
			expected: Config{
				CronSchedule: "*/10 * * * * *",
				Watches:      []WatchEntry{},
			},
		},
		{
			name:    "cron_schedule defaults to every 5 seconds when omitted",
			content: "watches: []",
			expected: Config{
				CronSchedule: watcherconfig.DefaultCronSchedule,
				Watches:      []WatchEntry{},
			},
		},
		{
			name:    "invalid YAML returns error",
			content: "{ this is: [not valid yaml",
			errFunc: require.Error,
		},
		{
			name: "empty path in watch entry returns error",
			content: `
watches:
  - path: ""
    media_type: movie
`,
			errFunc: require.Error,
		},
		{
			name: "empty media_type in watch entry returns error",
			content: `
watches:
  - path: /watch/movies
    media_type: ""
`,
			errFunc: require.Error,
		},
		{
			name: "unrecognized media_type returns error",
			content: `
watches:
  - path: /watch/movies
    media_type: UnknownType
`,
			errFunc: require.Error,
		},
		{
			name: "invalid cron expression returns error",
			content: `
cron_schedule: "* * * *"
watches: []
`,
			errFunc: require.Error,
		},
		{
			name: "five-field cron expression returns error",
			content: `
cron_schedule: "* * * * *"
watches: []
`,
			errFunc: require.Error,
		},
		{
			name: "seven-field cron expression returns error",
			content: `
cron_schedule: "* * * * * * *"
watches: []
`,
			errFunc: require.Error,
		},
		{
			name: "valid 6-field cron expression is accepted",
			content: `
cron_schedule: "0 30 9 * * *"
watches: []
`,
			expected: Config{
				CronSchedule: "0 30 9 * * *",
				Watches:      []WatchEntry{},
			},
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

			if err == nil {
				assert.Equal(t, tt.expected, *cfg)
			}
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
