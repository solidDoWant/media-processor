// Package medialib provides higher-level abstractions for media library services.
package medialib

import (
	"context"
	"errors"

	"github.com/invopop/jsonschema"
)

// ErrNotFound is returned when a media item is not found in the library.
var ErrNotFound = errors.New("not found in library")

// MediaType identifies whether a media file is a movie or a TV show episode.
type MediaType string

const (
	// MovieType identifies movie files.
	MovieType MediaType = "movie"
	// ShowType identifies TV show episode files.
	ShowType MediaType = "show"
)

// JSONSchema returns a JSON Schema for MediaType restricting values to the valid types.
func (MediaType) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{string(MovieType), string(ShowType)},
	}
}

// Movie represents a movie entry in a movie library service.
type Movie struct {
	// ID is the internal database ID assigned by the backing movie library service.
	ID    int64
	Title string
	Year  int
}

// Episode represents an episode entry in a TV library service.
type Episode struct {
	// ID is the internal database ID assigned by the backing TV library service.
	ID int64
	// SeriesID is the internal database ID of the series this episode belongs to.
	// Use this with EpisodeLibrary.RefreshSeries to trigger a library rescan.
	SeriesID      int64
	SeriesTitle   string
	SeasonNumber  int
	EpisodeNumber int
}

// MovieLibrary provides operations for movie media items.
type MovieLibrary interface {
	GetMovieByFilePath(ctx context.Context, path string) (Movie, error)
	RefreshMovie(ctx context.Context, id int64) error
}

// EpisodeLibrary provides operations for episode media items.
type EpisodeLibrary interface {
	GetEpisodeByFilePath(ctx context.Context, path string) (Episode, error)
	RefreshSeries(ctx context.Context, seriesID int64) error
}
