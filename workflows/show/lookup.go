package show

import (
	"context"
	"fmt"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// lookupOutput is the output of the lookup step.
type lookupOutput struct {
	SeriesID int64 `json:"series_id"`
}

// runLookup identifies the episode at filePath in Sonarr. It returns an error
// wrapping medialib.ErrNotFound if the file is not recognised.
func runLookup(ctx context.Context, filePath string, sonarrClient medialib.EpisodeLibrary) (lookupOutput, error) {
	episode, err := sonarrClient.GetEpisodeByFilePath(ctx, filePath)
	if err != nil {
		return lookupOutput{}, fmt.Errorf("lookup episode: %w", err)
	}

	return lookupOutput{SeriesID: episode.SeriesID}, nil
}
