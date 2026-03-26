package media

import (
	"context"
	"fmt"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// LibraryClient is a unified interface for looking up a media file and refreshing
// its entry in the backing library service (Radarr for movies, Sonarr for shows).
// It abstracts the type-specific calls so workflow steps need no media-type switch.
type LibraryClient interface {
	// GetIDByFilePath looks up the media item at path and returns the ID needed
	// to trigger a library refresh. For movies this is the movie ID; for shows
	// this is the series ID.
	GetIDByFilePath(ctx context.Context, path string) (int64, error)
	// Refresh triggers the backing library service to rescan the item with the
	// given ID.
	Refresh(ctx context.Context, id int64) error
}

// selectLibrary returns the LibraryClient corresponding to mediaType, using
// radarrClient for movies and sonarrClient for TV episodes. It is the single
// dispatch point for media-type selection in the workflow.
func selectLibrary(mediaType medialib.MediaType, radarrClient medialib.MovieLibrary, sonarrClient medialib.EpisodeLibrary) (LibraryClient, error) {
	switch mediaType {
	case medialib.MovieType:
		return &movieLibraryClient{radarrClient}, nil
	case medialib.ShowType:
		return &episodeLibraryClient{sonarrClient}, nil
	default:
		return nil, fmt.Errorf("unknown media type %q", mediaType)
	}
}

// movieLibraryClient adapts medialib.MovieLibrary to the LibraryClient interface.
type movieLibraryClient struct{ client medialib.MovieLibrary }

func (a *movieLibraryClient) GetIDByFilePath(ctx context.Context, path string) (int64, error) {
	movie, err := a.client.GetMovieByFilePath(ctx, path)
	if err != nil {
		return 0, err
	}
	return movie.ID, nil
}

func (a *movieLibraryClient) Refresh(ctx context.Context, id int64) error {
	return a.client.RefreshMovie(ctx, id)
}

// episodeLibraryClient adapts medialib.EpisodeLibrary to the LibraryClient interface.
type episodeLibraryClient struct{ client medialib.EpisodeLibrary }

func (a *episodeLibraryClient) GetIDByFilePath(ctx context.Context, path string) (int64, error) {
	episode, err := a.client.GetEpisodeByFilePath(ctx, path)
	if err != nil {
		return 0, err
	}
	return episode.SeriesID, nil
}

func (a *episodeLibraryClient) Refresh(ctx context.Context, id int64) error {
	return a.client.RefreshSeries(ctx, id)
}
