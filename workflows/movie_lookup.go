package workflows

import (
	"context"
	"fmt"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// lookupOutput is the output of the lookup step.
type lookupOutput struct {
	MovieID int64 `json:"movie_id"`
}

// runLookup identifies the movie at filePath in Radarr. It returns an error
// wrapping medialib.ErrNotFound if the file is not recognised.
func runLookup(ctx context.Context, filePath string, radarrClient medialib.MovieLibrary) (lookupOutput, error) {
	movie, err := radarrClient.GetMovieByFilePath(ctx, filePath)
	if err != nil {
		return lookupOutput{}, fmt.Errorf("lookup movie: %w", err)
	}

	return lookupOutput{MovieID: movie.ID}, nil
}
