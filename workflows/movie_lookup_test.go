package workflows

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// stubMovieLibrary implements medialib.MovieLibrary for testing.
type stubMovieLibrary struct {
	movie medialib.Movie
	err   error
}

func (s *stubMovieLibrary) GetMovieByFilePath(_ context.Context, _ string) (medialib.Movie, error) {
	return s.movie, s.err
}

func (s *stubMovieLibrary) RefreshMovie(_ context.Context, _ int64) error {
	return s.err
}

func TestRunLookup(t *testing.T) {
	tests := []struct {
		name     string
		stub     *stubMovieLibrary
		expected lookupOutput
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "found movie returns its ID",
			stub:     &stubMovieLibrary{movie: medialib.Movie{ID: 42, Title: "Interstellar", Year: 2014}},
			expected: lookupOutput{MovieID: 42},
			errFunc:  require.NoError,
		},
		{
			name:    "library returns ErrNotFound propagates error",
			stub:    &stubMovieLibrary{err: medialib.ErrNotFound},
			errFunc: require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runLookup(t.Context(), "/media/Interstellar.mkv", tt.stub)

			tt.errFunc(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}
