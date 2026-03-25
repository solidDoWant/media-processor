package show

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

func TestRunLookup(t *testing.T) {
	tests := []struct {
		name     string
		stub     *stubEpisodeLibrary
		expected lookupOutput
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "found episode returns its series ID",
			stub:     &stubEpisodeLibrary{episode: medialib.Episode{ID: 10, SeriesID: 42, SeriesTitle: "Breaking Bad"}},
			expected: lookupOutput{SeriesID: 42},
			errFunc:  require.NoError,
		},
		{
			name:    "library returns ErrNotFound propagates error",
			stub:    &stubEpisodeLibrary{err: medialib.ErrNotFound},
			errFunc: require.Error,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runLookup(t.Context(), "/media/shows/Breaking.Bad.S01E01.mkv", tt.stub)

			tt.errFunc(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}
