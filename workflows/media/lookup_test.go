package media

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLibraryClient implements LibraryClient for testing.
type stubLibraryClient struct {
	id           int64
	err          error
	refreshCalls []int64
}

func (s *stubLibraryClient) GetIDByFilePath(_ context.Context, _ string) (int64, error) {
	return s.id, s.err
}

func (s *stubLibraryClient) Refresh(_ context.Context, id int64) error {
	s.refreshCalls = append(s.refreshCalls, id)
	return s.err
}

func TestRunLookup(t *testing.T) {
	tests := []struct {
		name     string
		stub     *stubLibraryClient
		expected lookupOutput
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "found returns ID",
			stub:     &stubLibraryClient{id: 42},
			expected: lookupOutput{MediaID: 42},
			errFunc:  require.NoError,
		},
		{
			name:    "ErrNotFound propagates",
			stub:    &stubLibraryClient{err: errors.New("not found")},
			errFunc: require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runLookup(t.Context(), "/media/file.mkv", tt.stub)
			tt.errFunc(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}
