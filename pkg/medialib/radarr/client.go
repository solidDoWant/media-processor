// Package radarr provides a Radarr client implementing medialib.MovieLibrary.
package radarr

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"golift.io/starr"
	radarrlib "golift.io/starr/radarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// Compile-time assertion that *Client implements medialib.MovieLibrary.
var _ medialib.MovieLibrary = (*Client)(nil)

// Config holds the configuration for a Radarr client.
type Config struct {
	URL    string
	APIKey string
	// LocalPathPrefix and RemotePathPrefix enable optional path translation.
	// When set, paths starting with LocalPathPrefix are rewritten to use
	// RemotePathPrefix before being matched against Radarr's stored paths.
	// This handles cases where the worker and Radarr use different mount points
	// for the same storage.
	LocalPathPrefix  string
	RemotePathPrefix string
}

// Client is a Radarr client implementing medialib.MovieLibrary.
type Client struct {
	cfg    Config
	radarr *radarrlib.Radarr
}

// New creates a new Radarr client.
func New(cfg Config) *Client {
	s := starr.New(cfg.APIKey, cfg.URL, 0)
	return &Client{cfg: cfg, radarr: radarrlib.New(s)}
}

// GetMovieByFilePath returns the movie whose file matches path.
// Returns medialib.ErrNotFound if no match is found.
func (c *Client) GetMovieByFilePath(ctx context.Context, path string) (medialib.Movie, error) {
	if c.cfg.LocalPathPrefix != "" {
		if after, ok := strings.CutPrefix(path, c.cfg.LocalPathPrefix); ok {
			path = c.cfg.RemotePathPrefix + after
		}
	}
	path = filepath.Clean(path)

	movies, err := c.radarr.GetMovieContext(ctx, &radarrlib.GetMovie{})
	if err != nil {
		return medialib.Movie{}, fmt.Errorf("list movies: %w", err)
	}

	for _, m := range movies {
		if m.MovieFile != nil && m.MovieFile.Path == path {
			return medialib.Movie{
				ID:    m.ID,
				Title: m.Title,
				Year:  m.Year,
			}, nil
		}
	}

	return medialib.Movie{}, medialib.ErrNotFound
}

// RefreshMovie triggers a Radarr library rescan for the given movie ID.
func (c *Client) RefreshMovie(ctx context.Context, id int64) error {
	_, err := c.radarr.SendCommandContext(ctx, &radarrlib.CommandRequest{
		Name:     "RefreshMovie",
		MovieIDs: []int64{id},
	})
	if err != nil {
		return fmt.Errorf("refresh movie %d: %w", id, err)
	}

	return nil
}
