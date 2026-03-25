package workflows

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunTranscode(t *testing.T) {
	tests := []struct {
		name      string
		setupPath func(t *testing.T) string
		probe     probeOutput
		errFunc   require.ErrorAssertionFunc
		check     func(t *testing.T, result transcodeOutput)
	}{
		{
			name:      "valid H.264 MP4 transcodes to MKV temp file",
			setupPath: copyTestVideo,
			probe: probeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
			},
			errFunc: require.NoError,
			check: func(t *testing.T, result transcodeOutput) {
				assert.NotEmpty(t, result.TempPath)
				_, err := os.Stat(result.TempPath)
				require.NoError(t, err, "transcoded file should exist")
				assert.Equal(t, ".mp4", filepath.Ext(result.TempPath))

				// Verify temp dir is workflow-run-scoped
				assert.Contains(t, result.TempPath, "media-processor-")

				// Clean up temp dir
				_ = os.RemoveAll(filepath.Dir(result.TempPath))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setupPath(t)
			input := MovieInput{FilePath: path}

			result, err := runTranscode(t.Context(), input, tt.probe, t.Name())

			tt.errFunc(t, err)
			if tt.check != nil {
				tt.check(t, result)
			}
		})
	}
}
