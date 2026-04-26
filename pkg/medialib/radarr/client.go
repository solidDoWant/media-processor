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

// ImportByFilePath implements medialib.ArrLibrary. It sends a DownloadedMoviesScan command
// for path, causing Radarr to import the file at path into the library.
func (c *Client) ImportByFilePath(ctx context.Context, path string) error {
	requestPayload := struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{
		Name: "DownloadedMoviesScan",
		Path: path,
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(requestPayload); err != nil {
		return fmt.Errorf("encode command: %w", err)
	}

	var resp radarrlib.CommandResponse
	if err := c.radarr.PostInto(ctx, starr.Request{URI: radarrlib.APIver + "/command", Body: &body}, &resp); err != nil {
		return fmt.Errorf("scan downloaded movies at %q: %w", path, err)
	}

	return nil
}
