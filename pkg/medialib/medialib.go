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
	// Used internally by the Sonarr client for series-level refresh.
	SeriesID      int64
	Title         string
	Year          int
	SeriesTitle   string
	SeasonNumber  int
	EpisodeNumber int
}

// MediaInfo exposes typed getters for per-media metadata.
type MediaInfo interface {
	GetID() int64
	GetTitle() string
	GetYear() int
	GetSeriesTitle() string // movies return ""
	GetSeasonNumber() int   // movies return 0
	GetEpisodeNumber() int  // movies return 0
}

// GetID returns the movie's ID.
func (m *Movie) GetID() int64 { return m.ID }

// GetTitle returns the movie's title.
func (m *Movie) GetTitle() string { return m.Title }

// GetYear returns the movie's release year.
func (m *Movie) GetYear() int { return m.Year }

// GetSeriesTitle returns "" for movies.
func (m *Movie) GetSeriesTitle() string { return "" }

// GetSeasonNumber returns 0 for movies.
func (m *Movie) GetSeasonNumber() int { return 0 }

// GetEpisodeNumber returns 0 for movies.
func (m *Movie) GetEpisodeNumber() int { return 0 }

// GetID returns the episode's ID.
func (e *Episode) GetID() int64 { return e.ID }

// GetTitle returns the episode's title.
func (e *Episode) GetTitle() string { return e.Title }

// GetYear returns the episode's year.
func (e *Episode) GetYear() int { return e.Year }

// GetSeriesTitle returns the episode's series title.
func (e *Episode) GetSeriesTitle() string { return e.SeriesTitle }

// GetSeasonNumber returns the episode's season number.
func (e *Episode) GetSeasonNumber() int { return e.SeasonNumber }

// GetEpisodeNumber returns the episode's episode number.
func (e *Episode) GetEpisodeNumber() int { return e.EpisodeNumber }

// MovieLibrary provides operations for movie media items.
type MovieLibrary interface {
	GetMovieByFilePath(ctx context.Context, path string) (Movie, error)
}

// EpisodeLibrary provides operations for episode media items.
type EpisodeLibrary interface {
	GetEpisodeByFilePath(ctx context.Context, path string) (Episode, error)
}

// ArrLibrary is a unified interface for refreshing a media item in the backing
// library service (Radarr for movies, Sonarr for shows). It abstracts the
// type-specific lookup-and-refresh calls so workflow steps need no media-type switch.
type ArrLibrary interface {
	// RefreshByFilePath looks up the media item at path in the backing library
	// service and triggers a rescan. For Sonarr, the rescan is at series level
	// (Sonarr does not support episode-level refresh).
	RefreshByFilePath(ctx context.Context, path string) error
	// GetInfo returns structured media metadata for the item at path.
	// Returns ErrNotFound if no item is identified.
	GetInfo(ctx context.Context, path string) (MediaInfo, error)
}
