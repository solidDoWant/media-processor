package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

func TestRunLookup(t *testing.T) {
	tests := []struct {
		name           string
		mediaType      MediaType
		movieLibrary   *stubMovieLibrary
		episodeLibrary *stubEpisodeLibrary
		expected       lookupOutput
		errFunc        require.ErrorAssertionFunc
	}{
		{
			name:           "movie: found returns movie ID",
			mediaType:      Movie,
			movieLibrary:   &stubMovieLibrary{movie: medialib.Movie{ID: 42, Title: "Interstellar"}},
			episodeLibrary: &stubEpisodeLibrary{},
			expected:       lookupOutput{MediaID: 42},
			errFunc:        require.NoError,
		},
		{
			name:           "movie: ErrNotFound propagates",
			mediaType:      Movie,
			movieLibrary:   &stubMovieLibrary{err: medialib.ErrNotFound},
			episodeLibrary: &stubEpisodeLibrary{},
			errFunc:        require.Error,
		},
		{
			name:           "show: found returns series ID",
			mediaType:      Show,
			movieLibrary:   &stubMovieLibrary{},
			episodeLibrary: &stubEpisodeLibrary{episode: medialib.Episode{ID: 10, SeriesID: 42, SeriesTitle: "Breaking Bad"}},
			expected:       lookupOutput{MediaID: 42},
			errFunc:        require.NoError,
		},
		{
			name:           "show: ErrNotFound propagates",
			mediaType:      Show,
			movieLibrary:   &stubMovieLibrary{},
			episodeLibrary: &stubEpisodeLibrary{err: medialib.ErrNotFound},
			errFunc:        require.Error,
		},
		{
			name:           "unknown media type returns error",
			mediaType:      MediaType("unknown"),
			movieLibrary:   &stubMovieLibrary{},
			episodeLibrary: &stubEpisodeLibrary{},
			errFunc:        require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runLookup(t.Context(), "/media/file.mkv", tt.mediaType, tt.movieLibrary, tt.episodeLibrary)
			tt.errFunc(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}
