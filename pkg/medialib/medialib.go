// Package medialib provides higher-level abstractions for media library services.
package medialib

import (
	"context"
	"errors"

	"github.com/invopop/jsonschema"
)

// ErrNotFound is returned when a media item is not found in the library.
var ErrNotFound = errors.New("not found in library")

// ErrLibraryFileMismatch is returned by ImportByFilePath when the arr service
// already has a file for the requested media item but that file's size differs
// from expectedSize. This indicates the arr service's existing file is from a
// different import (not the file we just produced) and the service declined to
// replace it. Retrying will not help; the operator must resolve the conflict in
// the arr service before the workflow can succeed.
var ErrLibraryFileMismatch = errors.New("library already has a different file for this media item")

// MediaType identifies whether a media file is a movie or a TV show episode.
type MediaType string

const (
	// MovieType identifies movie files.
	MovieType MediaType = "movie"
	// ShowType identifies TV show episode files.
	ShowType MediaType = "show"
)

// AllMediaTypes returns the canonical list of valid MediaType values. It is the
// single source of truth shared by runtime validation and JSON Schema generation.
func AllMediaTypes() []MediaType {
	return []MediaType{MovieType, ShowType}
}

// JSONSchema returns a JSON Schema for MediaType restricting values to the valid types.
func (MediaType) JSONSchema() *jsonschema.Schema {
	all := AllMediaTypes()

	enum := make([]any, len(all))
	for i, mt := range all {
		enum[i] = string(mt)
	}

	return &jsonschema.Schema{
		Type:        "string",
		Enum:        enum,
		Description: "Whether the watched directory contains movies (\"movie\") or TV show episodes (\"show\").",
	}
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

// Compile-time assertions that *Movie and *Episode implement MediaInfo.
var _ MediaInfo = (*Movie)(nil)
var _ MediaInfo = (*Episode)(nil)

// Movie represents a movie entry in a movie library service. Construct with NewMovie.
type Movie struct {
	id    int64
	title string
	year  int
}

// NewMovie returns a Movie populated with the supplied metadata.
// id is the internal database ID assigned by the backing movie library service.
func NewMovie(id int64, title string, year int) *Movie {
	return &Movie{id: id, title: title, year: year}
}

// GetID returns the movie's ID.
func (m *Movie) GetID() int64 { return m.id }

// GetTitle returns the movie's title.
func (m *Movie) GetTitle() string { return m.title }

// GetYear returns the movie's release year.
func (m *Movie) GetYear() int { return m.year }

// GetSeriesTitle returns "" for movies.
func (m *Movie) GetSeriesTitle() string { return "" }

// GetSeasonNumber returns 0 for movies.
func (m *Movie) GetSeasonNumber() int { return 0 }

// GetEpisodeNumber returns 0 for movies.
func (m *Movie) GetEpisodeNumber() int { return 0 }

// Episode represents an episode entry in a TV library service. Construct with NewEpisode.
type Episode struct {
	id            int64
	seriesID      int64
	title         string
	year          int
	seriesTitle   string
	seasonNumber  int
	episodeNumber int
}

// NewEpisode returns an Episode populated with the supplied metadata.
// id and seriesID are the internal database IDs assigned by the backing TV
// library service. year is the series premiere year.
func NewEpisode(id, seriesID int64, title, seriesTitle string, year, seasonNumber, episodeNumber int) *Episode {
	return &Episode{
		id:            id,
		seriesID:      seriesID,
		title:         title,
		year:          year,
		seriesTitle:   seriesTitle,
		seasonNumber:  seasonNumber,
		episodeNumber: episodeNumber,
	}
}

// SeriesID returns the internal database ID of the series this episode belongs
// to. Used by the Sonarr client for series-level lookups (e.g. fetching the
// series poster).
func (e *Episode) SeriesID() int64 { return e.seriesID }

// GetID returns the episode's ID.
func (e *Episode) GetID() int64 { return e.id }

// GetTitle returns the episode's title.
func (e *Episode) GetTitle() string { return e.title }

// GetYear returns the series premiere year.
func (e *Episode) GetYear() int { return e.year }

// GetSeriesTitle returns the episode's series title.
func (e *Episode) GetSeriesTitle() string { return e.seriesTitle }

// GetSeasonNumber returns the episode's season number.
func (e *Episode) GetSeasonNumber() int { return e.seasonNumber }

// GetEpisodeNumber returns the episode's episode number.
func (e *Episode) GetEpisodeNumber() int { return e.episodeNumber }

// ArrLibrary is a unified interface for refreshing a media item in the backing
// library service (Radarr for movies, Sonarr for shows). It abstracts the
// type-specific lookup-and-refresh calls so workflow steps need no media-type switch.
type ArrLibrary interface {
	// ImportByFilePath translates path to the arr service's view and sends a
	// DownloadedMoviesScan (Radarr) or DownloadedEpisodesScan (Sonarr) command,
	// triggering the arr service's normal download-completion import pipeline.
	// expectedSize is the size of the local source file in bytes. It is used
	// as a post-check tiebreaker when the scan command finishes with no
	// successful imports: if the library item's stored file matches
	// expectedSize, the import is treated as having succeeded (typically
	// because the arr service's own completed-download handler raced our
	// scan). A non-positive expectedSize disables the post-check.
	// Returns ErrLibraryFileMismatch (wrapped) when the arr service already
	// has a file of a different size and declined to replace it — this
	// condition will not resolve on retry.
	ImportByFilePath(ctx context.Context, path string, expectedSize int64) error
	// GetInfo returns structured media metadata for the item at path.
	// Returns ErrNotFound if no item is identified.
	GetInfo(ctx context.Context, path string) (MediaInfo, error)
	// GetPosterImage returns the raw poster image bytes and MIME type for the
	// media item at path. Returns ErrNotFound if the media item at path cannot
	// be identified in the library. Returns nil bytes and empty MIME type (with
	// no error) when a media item is found but no poster is available or the
	// image type is not JPEG or PNG. Other errors indicate the library service
	// is unreachable or returned an unexpected failure.
	GetPosterImage(ctx context.Context, path string) (imageBytes []byte, mimeType string, err error)
}
