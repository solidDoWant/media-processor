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
  - name: movies
    path: /watch/movies
    mediaType: movie
  - name: shows
    path: /watch/shows
    mediaType: show
`,
			expected: Config{
				CronSchedule: watcherconfig.DefaultCronSchedule,
				Watches: []WatchEntry{
					{Name: "movies", Path: "/watch/movies", MediaType: medialib.MovieType},
					{Name: "shows", Path: "/watch/shows", MediaType: medialib.ShowType},
				},
			},
		},
		{
			name: "custom cronSchedule is parsed",
			content: `
cronSchedule: "*/10 * * * * *"
watches: []
`,
			expected: Config{
				CronSchedule: "*/10 * * * * *",
				Watches:      []WatchEntry{},
			},
		},
		{
			name:    "cronSchedule defaults to every 5 seconds when omitted",
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
			name: "omitted name in watch entry returns error",
			content: `
watches:
  - path: /watch/movies
    mediaType: movie
`,
			errFunc: require.Error,
		},
		{
			name: "empty string name in watch entry returns error",
			content: `
watches:
  - name: ""
    path: /watch/movies
    mediaType: movie
`,
			errFunc: require.Error,
		},
		{
			name: "empty path in watch entry returns error",
			content: `
watches:
  - name: movies
    path: ""
    mediaType: movie
`,
			errFunc: require.Error,
		},
		{
			name: "empty mediaType in watch entry returns error",
			content: `
watches:
  - name: movies
    path: /watch/movies
    mediaType: ""
`,
			errFunc: require.Error,
		},
		{
			name: "unrecognized mediaType returns error",
			content: `
watches:
  - name: movies
    path: /watch/movies
    mediaType: UnknownType
`,
			errFunc: require.Error,
		},
		{
			name: "invalid cron expression returns error",
			content: `
cronSchedule: "* * * *"
watches: []
`,
			errFunc: require.Error,
		},
		{
			name: "five-field cron expression returns error",
			content: `
cronSchedule: "* * * * *"
watches: []
`,
			errFunc: require.Error,
		},
		{
			name: "seven-field cron expression returns error",
			content: `
cronSchedule: "* * * * * * *"
watches: []
`,
			errFunc: require.Error,
		},
		{
			name: "valid 6-field cron expression is accepted",
			content: `
cronSchedule: "0 30 9 * * *"
watches: []
`,
			expected: Config{
				CronSchedule: "0 30 9 * * *",
				Watches:      []WatchEntry{},
			},
		},
		{
			name: "preserveSource true is parsed and set on watch entry",
			content: `
watches:
  - name: movies
    path: /watch/movies
    media_type: movie
    preserveSource: true
`,
			expected: Config{
				CronSchedule: watcherconfig.DefaultCronSchedule,
				Watches: []WatchEntry{
					{Name: "movies", Path: "/watch/movies", MediaType: medialib.MovieType, PreserveSource: true},
				},
			},
		},
		{
			name: "invalid regex in ignorePatterns returns error",
			content: `
watches:
  - name: movies
    path: /watch/movies
    mediaType: movie
    ignorePatterns:
      - "[unclosed"
`,
			errFunc: require.Error,
		},
		{
			name: "integer ignorePatterns entry returns error",
			content: `
watches:
  - name: movies
    path: /watch/movies
    mediaType: movie
    ignorePatterns:
      - 123
`,
			errFunc: require.Error,
		},
		{
			name: "empty string ignorePatterns entry returns error",
			content: `
watches:
  - name: movies
    path: /watch/movies
    mediaType: movie
    ignorePatterns:
      - ""
`,
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

// TestLoadConfig_NullIgnorePatternEntryDropped verifies that yaml.v3 silently drops a null
// entry in ignorePatterns rather than treating it as an empty pattern that matches everything.
func TestLoadConfig_NullIgnorePatternEntryDropped(t *testing.T) {
	t.Parallel()

	content := `
watches:
  - name: movies
    path: /watch/movies
    mediaType: movie
    ignorePatterns:
      - null
`
	path := writeTempConfig(t, content)
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Watches, 1)
	assert.Empty(t, cfg.Watches[0].IgnorePatterns, "null entry should be silently dropped by yaml.v3")
}

// TestLoadConfig_IgnorePatternsParsedAndCompiled verifies that valid ignorePatterns entries
// are compiled from YAML strings and produce working regular expressions.
func TestLoadConfig_IgnorePatternsParsedAndCompiled(t *testing.T) {
	t.Parallel()

	content := `
watches:
  - name: movies
    path: /watch/movies
    mediaType: movie
    ignorePatterns:
      - \.!qB$
      - (^|/)_unpack(/|$)
`
	path := writeTempConfig(t, content)
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Watches, 1)
	require.Len(t, cfg.Watches[0].IgnorePatterns, 2)
	assert.Equal(t, `\.!qB$`, cfg.Watches[0].IgnorePatterns[0].String())
	assert.Equal(t, `(^|/)_unpack(/|$)`, cfg.Watches[0].IgnorePatterns[1].String())
	assert.True(t, cfg.Watches[0].IgnorePatterns[0].MatchString("/media/video.mkv.!qB"), "should match .!qB suffix")
	assert.False(t, cfg.Watches[0].IgnorePatterns[0].MatchString("/media/video.mkv"), "should not match clean path")
	assert.True(t, cfg.Watches[0].IgnorePatterns[1].MatchString("/media/_unpack/video.mkv"), "should match _unpack subtree")
	assert.False(t, cfg.Watches[0].IgnorePatterns[1].MatchString("/media/video.mkv"), "should not match clean path")
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}
