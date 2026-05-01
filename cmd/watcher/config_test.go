package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
  - name: shows
    watchedPath: /watch/shows
    mediaType: show
    output:
      path: /out/shows
`,
			expected: Config{
				ScanInterval: watcherconfig.DefaultScanInterval,
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: "/watch/movies", MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: "/out/movies"}},
					{Name: "shows", WatchedPath: "/watch/shows", MediaType: medialib.ShowType, Output: watcherconfig.WatchEntryOutput{Path: "/out/shows"}},
				},
			},
		},
		{
			name: "custom scanInterval is parsed",
			content: `
scanInterval: 10s
watches: []
`,
			expected: Config{
				ScanInterval: watcherconfig.Interval(10 * time.Second),
				Watches:      []WatchEntry{},
			},
		},
		{
			name:    "scanInterval defaults to 5 seconds when omitted",
			content: "watches: []",
			expected: Config{
				ScanInterval: watcherconfig.DefaultScanInterval,
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
  - watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
`,
			errFunc: require.Error,
		},
		{
			name: "empty string name in watch entry returns error",
			content: `
watches:
  - name: ""
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
`,
			errFunc: require.Error,
		},
		{
			name: "omitted watchedPath in watch entry returns error",
			content: `
watches:
  - name: movies
    mediaType: movie
    output:
      path: /out/movies
`,
			errFunc: require.Error,
		},
		{
			name: "empty watchedPath in watch entry returns error",
			content: `
watches:
  - name: movies
    watchedPath: ""
    mediaType: movie
    output:
      path: /out/movies
`,
			errFunc: require.Error,
		},
		{
			name: "omitted output.path in watch entry returns error",
			content: `
watches:
  - name: movies
    watchedPath: /watch/movies
    mediaType: movie
`,
			errFunc: require.Error,
		},
		{
			name: "empty output.path in watch entry returns error",
			content: `
watches:
  - name: movies
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: ""
`,
			errFunc: require.Error,
		},
		{
			name: "output.remotePath is optional",
			content: `
watches:
  - name: movies
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
      remotePath: /remote/movies
`,
			expected: Config{
				ScanInterval: watcherconfig.DefaultScanInterval,
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: "/watch/movies", MediaType: medialib.MovieType, Output: watcherconfig.WatchEntryOutput{Path: "/out/movies", RemotePath: "/remote/movies"}},
				},
			},
		},
		{
			name: "empty mediaType in watch entry returns error",
			content: `
watches:
  - name: movies
    watchedPath: /watch/movies
    mediaType: ""
    output:
      path: /out/movies
`,
			errFunc: require.Error,
		},
		{
			name: "unrecognized mediaType returns error",
			content: `
watches:
  - name: movies
    watchedPath: /watch/movies
    mediaType: UnknownType
    output:
      path: /out/movies
`,
			errFunc: require.Error,
		},
		{
			name: "invalid scanInterval returns error",
			content: `
scanInterval: "not-a-duration"
watches: []
`,
			errFunc: require.Error,
		},
		{
			name: "preserveSource true is parsed and set on watch entry",
			content: `
watches:
  - name: movies
    watchedPath: /watch/movies
    mediaType: movie
    preserveSource: true
    output:
      path: /out/movies
`,
			expected: Config{
				ScanInterval: watcherconfig.DefaultScanInterval,
				Watches: []WatchEntry{
					{Name: "movies", WatchedPath: "/watch/movies", MediaType: medialib.MovieType, PreserveSource: true, Output: watcherconfig.WatchEntryOutput{Path: "/out/movies"}},
				},
			},
		},
		{
			name: "invalid regex in ignorePatterns returns error",
			content: `
watches:
  - name: movies
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
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
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
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
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
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
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
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
    watchedPath: /watch/movies
    mediaType: movie
    output:
      path: /out/movies
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
