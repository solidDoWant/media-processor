package workflows

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCleanup(t *testing.T) {
	tests := []struct {
		name        string
		setupPath   func(t *testing.T) string
		errFunc     require.ErrorAssertionFunc
		fileDeleted bool
	}{
		{
			name: "existing file is deleted",
			setupPath: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "source.mkv")
				require.NoError(t, os.WriteFile(p, []byte("data"), 0o600))
				return p
			},
			errFunc:     require.NoError,
			fileDeleted: true,
		},
		{
			name: "non-existent path returns no error",
			setupPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist.mkv")
			},
			errFunc:     require.NoError,
			fileDeleted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setupPath(t)

			err := runCleanup(path)

			tt.errFunc(t, err)
			if tt.fileDeleted {
				_, statErr := os.Stat(path)
				assert.True(t, os.IsNotExist(statErr), "expected file to be deleted")
			}
		})
	}
}
