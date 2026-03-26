package media

import (
	"context"
	"fmt"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// lookupOutput is the output of the lookup step.
type lookupOutput struct {
	MediaID int64 `json:"media_id"`
}

// runLookup identifies the media file at filePath in the appropriate library service
// based on mediaType. It returns an error wrapping medialib.ErrNotFound if the file
// is not recognised.
func runLookup(ctx context.Context, filePath string, mediaType MediaType, radarrClient medialib.MovieLibrary, sonarrClient medialib.EpisodeLibrary) (lookupOutput, error) {
	switch mediaType {
	case Movie:
		movie, err := radarrClient.GetMovieByFilePath(ctx, filePath)
		if err != nil {
			return lookupOutput{}, fmt.Errorf("lookup movie: %w", err)
		}
		return lookupOutput{MediaID: movie.ID}, nil
	case Show:
		episode, err := sonarrClient.GetEpisodeByFilePath(ctx, filePath)
		if err != nil {
			return lookupOutput{}, fmt.Errorf("lookup episode: %w", err)
		}
		return lookupOutput{MediaID: episode.SeriesID}, nil
	default:
		return lookupOutput{}, fmt.Errorf("unknown media type %q", mediaType)
	}
}
