// Package radarr provides a Radarr client implementing medialib.MovieLibrary.
package radarr

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"golift.io/starr"
	radarrlib "golift.io/starr/radarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// Compile-time assertions that *Client implements medialib.MovieLibrary and medialib.ArrLibrary.
var _ medialib.MovieLibrary = (*Client)(nil)
var _ medialib.ArrLibrary = (*Client)(nil)

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

// GetMovieByFilePath returns the movie identified by parsing the file path.
// Uses Radarr's parse endpoint, so it works for pre-import files.
// Returns medialib.ErrNotFound if no movie is identified.
func (c *Client) GetMovieByFilePath(ctx context.Context, path string) (medialib.Movie, error) {
	if c.cfg.LocalPathPrefix != "" {
		if after, ok := strings.CutPrefix(path, c.cfg.LocalPathPrefix); ok {
			path = c.cfg.RemotePathPrefix + after
		}
	}
	path = filepath.Clean(path)

	// Guard against path traversal: if a remote prefix is configured, reject
	// any path that escapes it after translation and cleaning.
	if c.cfg.RemotePathPrefix != "" {
		cleanPrefix := filepath.Clean(c.cfg.RemotePathPrefix)
		if !strings.HasPrefix(path, cleanPrefix+string(filepath.Separator)) {
			return medialib.Movie{}, fmt.Errorf("path %q is outside configured remote prefix %q", path, cleanPrefix)
		}
	}

	movie, err := c.parseFilePath(ctx, path)
	if err != nil {
		return medialib.Movie{}, fmt.Errorf("parse file path: %w", err)
	}

	if movie == nil {
		return medialib.Movie{}, medialib.ErrNotFound
	}

	return medialib.Movie{
		ID:    movie.ID,
		Title: movie.Title,
		Year:  movie.Year,
	}, nil
}

// parseFilePath calls Radarr's /api/v3/parse endpoint to identify a movie
// from a file path. Returns nil if Radarr cannot identify the movie.
func (c *Client) parseFilePath(ctx context.Context, path string) (*radarrlib.Movie, error) {
	var output struct {
		Movie *radarrlib.Movie `json:"movie"`
	}

	q := make(url.Values)
	q.Set("path", path)

	req := starr.Request{URI: radarrlib.APIver + "/parse", Query: q}
	if err := c.radarr.GetInto(ctx, req, &output); err != nil {
		return nil, fmt.Errorf("api.Get(parse): %w", err)
	}

	return output.Movie, nil
}

// RefreshByFilePath implements medialib.ArrLibrary. It looks up the movie by
// file path and triggers a Radarr library rescan for that movie.
func (c *Client) RefreshByFilePath(ctx context.Context, path string) error {
	movie, err := c.GetMovieByFilePath(ctx, path)
	if err != nil {
		return err
	}

	_, err = c.radarr.SendCommandContext(ctx, &radarrlib.CommandRequest{
		Name:     "RefreshMovie",
		MovieIDs: []int64{movie.ID},
	})
	if err != nil {
		return fmt.Errorf("refresh movie %d: %w", movie.ID, err)
	}

	return nil
}
