// Package radarr provides a Radarr client implementing medialib.MovieLibrary.
package radarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"golift.io/starr"
	radarrlib "golift.io/starr/radarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/artwork"
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

// translatePath applies LocalPathPrefix/RemotePathPrefix translation to path and
// validates the result is within the configured remote prefix. Returns the
// translated path, or an error if the path escapes the prefix.
func (c *Client) translatePath(path string) (string, error) {
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
			return "", fmt.Errorf("path %q is outside configured remote prefix %q", path, cleanPrefix)
		}
	}

	return path, nil
}

// GetMovieByFilePath returns the movie identified by parsing the file path.
// Uses Radarr's parse endpoint, so it works for pre-import files.
// Returns medialib.ErrNotFound if no movie is identified.
func (c *Client) GetMovieByFilePath(ctx context.Context, path string) (medialib.Movie, error) {
	var err error

	path, err = c.translatePath(path)
	if err != nil {
		return medialib.Movie{}, err
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
//
// The title parameter (filename stem without extension) is used instead of the
// path parameter because Radarr's parse endpoint matches path against library
// paths only, returning 204 No Content for download paths that haven't been
// imported yet.
func (c *Client) parseFilePath(ctx context.Context, path string) (*radarrlib.Movie, error) {
	var output struct {
		Movie *radarrlib.Movie `json:"movie"`
	}

	base := filepath.Base(path)
	q := make(url.Values)
	q.Set("title", strings.TrimSuffix(base, filepath.Ext(base)))

	req := starr.Request{URI: radarrlib.APIver + "/parse", Query: q}
	if err := c.radarr.GetInto(ctx, req, &output); err != nil {
		return nil, fmt.Errorf("api.Get(parse): %w", err)
	}

	return output.Movie, nil
}

// GetPosterImage implements medialib.ArrLibrary. It returns the raw poster
// image bytes and MIME type for the movie at path. Returns nil bytes (no error)
// when no JPEG or PNG poster is available. Returns medialib.ErrNotFound if the
// path cannot be matched to a movie. Returns other errors if the library is
// unreachable or a Radarr API call fails.
func (c *Client) GetPosterImage(ctx context.Context, path string) ([]byte, string, error) {
	movie, err := c.GetMovieByFilePath(ctx, path)
	if err != nil {
		return nil, "", fmt.Errorf("get movie for poster: %w", err)
	}

	full, err := c.radarr.GetMovieByIDContext(ctx, movie.ID)
	if err != nil {
		return nil, "", fmt.Errorf("get movie details for poster: %w", err)
	}

	return artwork.FetchPosterImage(ctx, full.Images, c.cfg.URL, c.cfg.APIKey)
}

// GetInfo implements medialib.ArrLibrary. It returns structured metadata for
// the movie at path.
func (c *Client) GetInfo(ctx context.Context, path string) (medialib.MediaInfo, error) {
	movie, err := c.GetMovieByFilePath(ctx, path)
	if err != nil {
		return nil, err
	}

	return &movie, nil
}

// ImportByFilePath implements medialib.ArrLibrary. It translates path to
// Radarr's view and sends a DownloadedMoviesScan command for that path,
// causing Radarr to import the file at path into the library.
func (c *Client) ImportByFilePath(ctx context.Context, path string) error {
	translated, err := c.translatePath(path)
	if err != nil {
		return err
	}

	requestPayload := struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{
		Name: "DownloadedMoviesScan",
		Path: translated,
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(requestPayload); err != nil {
		return fmt.Errorf("encode command: %w", err)
	}

	var resp radarrlib.CommandResponse
	if err := c.radarr.PostInto(ctx, starr.Request{URI: radarrlib.APIver + "/command", Body: &body}, &resp); err != nil {
		return fmt.Errorf("scan downloaded movies at %q: %w", translated, err)
	}

	return nil
}
