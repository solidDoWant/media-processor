package workflows

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyFile(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) (src, dst string)
		errFunc require.ErrorAssertionFunc
		check   func(t *testing.T, dst string)
	}{
		{
			name: "copies file contents to destination",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				src := filepath.Join(dir, "src.mkv")
				require.NoError(t, os.WriteFile(src, []byte("video data"), 0o600))
				return src, filepath.Join(dir, "dst.mkv")
			},
			errFunc: require.NoError,
			check: func(t *testing.T, dst string) {
				data, err := os.ReadFile(dst)
				require.NoError(t, err)
				assert.Equal(t, []byte("video data"), data)
			},
		},
		{
			name: "non-existent source returns error",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return filepath.Join(dir, "missing.mkv"), filepath.Join(dir, "dst.mkv")
			},
			errFunc: require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, dst := tt.setup(t)

			err := copyFile(src, dst)

			tt.errFunc(t, err)
			if tt.check != nil {
				tt.check(t, dst)
			}
		})
	}
}

func TestRunMove(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) (input MovieInput, tc transcodeOutput, outputDir string)
		errFunc require.ErrorAssertionFunc
		check   func(t *testing.T, input MovieInput, outputDir string, tc transcodeOutput)
	}{
		{
			name: "moves transcoded file to output dir and removes temp dir",
			setup: func(t *testing.T) (MovieInput, transcodeOutput, string) {
				tempDir := t.TempDir()
				srcContent := []byte("transcoded video")
				tempPath := filepath.Join(tempDir, "movie.mkv")
				require.NoError(t, os.WriteFile(tempPath, srcContent, 0o600))

				outputDir := t.TempDir()
				return MovieInput{FilePath: "/input/movie.mkv"},
					transcodeOutput{TempPath: tempPath},
					outputDir
			},
			errFunc: require.NoError,
			check: func(t *testing.T, input MovieInput, outputDir string, tc transcodeOutput) {
				finalPath := filepath.Join(outputDir, filepath.Base(input.FilePath))
				data, err := os.ReadFile(finalPath)
				require.NoError(t, err)
				assert.Equal(t, []byte("transcoded video"), data)

				// temp dir should be removed
				_, statErr := os.Stat(filepath.Dir(tc.TempPath))
				assert.True(t, os.IsNotExist(statErr), "expected temp dir to be removed")
			},
		},
		{
			name: "non-existent temp file returns error",
			setup: func(t *testing.T) (MovieInput, transcodeOutput, string) {
				return MovieInput{FilePath: "/input/movie.mkv"},
					transcodeOutput{TempPath: filepath.Join(t.TempDir(), "missing.mkv")},
					t.TempDir()
			},
			errFunc: require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, tc, outputDir := tt.setup(t)

			err := runMove(input, tc, outputDir)

			tt.errFunc(t, err)
			if tt.check != nil {
				tt.check(t, input, outputDir, tc)
			}
		})
	}
}
