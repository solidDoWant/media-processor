package shared

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunProbe(t *testing.T) {
	tests := []struct {
		name        string
		setupPath   func(t *testing.T) string
		expected    ProbeOutput
		errFunc     require.ErrorAssertionFunc
		fileDeleted bool
	}{
		{
			name:      "valid H.264 MP4 returns codec, format, and audio streams",
			setupPath: copyTestVideo,
			expected: ProbeOutput{
				IsValidMedia: true,
				VideoCodec:   "h264",
				Format:       "mov,mp4,m4a,3gp,3g2,mj2",
				AudioStreams: []AudioStreamInfo{
					{StreamInfo: StreamInfo{Index: 1, Language: "und"}, ChannelCount: 2},
				},
			},
			errFunc:     require.NoError,
			fileDeleted: false,
		},
		{
			name: "non-media text file returns invalid and deletes file",
			setupPath: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "notavideo.txt")
				require.NoError(t, os.WriteFile(p, []byte("hello"), 0o600))
				return p
			},
			expected:    ProbeOutput{IsValidMedia: false},
			errFunc:     require.NoError,
			fileDeleted: true,
		},
		{
			name: "non-existent path returns invalid without error",
			setupPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist.mp4")
			},
			expected:    ProbeOutput{IsValidMedia: false},
			errFunc:     require.NoError,
			fileDeleted: false, // nothing to delete
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setupPath(t)

			got, err := RunProbe(t.Context(), path)

			tt.errFunc(t, err)
			assert.Equal(t, tt.expected, got)

			if tt.fileDeleted {
				_, statErr := os.Stat(path)
				assert.True(t, os.IsNotExist(statErr), "expected file to be deleted")
			}
		})
	}
}

func TestRunProbe_CancelledContextPropagatesError(t *testing.T) {
	// A cancelled context must not silently delete the file — it must propagate
	// the context error so the step fails and OnFailure fires.
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	path := copyTestVideo(t)

	_, err := RunProbe(ctx, path)
	require.ErrorIs(t, err, context.Canceled, "cancelled context should propagate as error, not delete the file")

	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "file must not be deleted on context cancellation")
}
