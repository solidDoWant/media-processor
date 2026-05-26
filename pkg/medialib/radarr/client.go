// Package radarr provides a Radarr client implementing medialib.ArrLibrary.
package radarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golift.io/starr"
	radarrlib "golift.io/starr/radarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/arrcommand"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/artwork"
)

// Compile-time assertion that *Client implements medialib.ArrLibrary.
var _ medialib.ArrLibrary = (*Client)(nil)

// Config holds the configuration for a Radarr client.
type Config struct {
	URL    string
	APIKey string
	// CommandPollInterval overrides the default poll interval for waiting on
	// command completion. Zero uses arrcommand.DefaultPollInterval.
	CommandPollInterval time.Duration
}

// Client is a Radarr client implementing medialib.ArrLibrary.
type Client struct {
	cfg    Config
	radarr *radarrlib.Radarr
}

// New creates a new Radarr client.
func New(cfg Config) *Client {
	s := starr.New(cfg.APIKey, cfg.URL, 0)
	return &Client{cfg: cfg, radarr: radarrlib.New(s)}
}

// getMovieByFilePath returns the movie identified by parsing the file path.
// Uses Radarr's parse endpoint, so it works for pre-import files.
// Returns medialib.ErrNotFound if no movie is identified.
func (c *Client) getMovieByFilePath(ctx context.Context, filePath string) (*medialib.Movie, error) {
	movie, err := c.parseFilePath(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("parse file path: %w", err)
	}

	if movie == nil {
		return nil, medialib.ErrNotFound
	}

	return medialib.NewMovie(movie.ID, movie.Title, movie.Year), nil
}

// parseFilePath calls Radarr's /api/v3/parse endpoint to identify a movie
// from a file path. Returns nil if Radarr cannot identify the movie.
//
// The title parameter (filename stem without extension) is used instead of the
// path parameter because Radarr's parse endpoint matches path against library
// paths only, returning 204 No Content for download paths that haven't been
// imported yet.
func (c *Client) parseFilePath(ctx context.Context, filePath string) (*radarrlib.Movie, error) {
	var output struct {
		Movie *radarrlib.Movie `json:"movie"`
	}

	base := filepath.Base(filePath)
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
func (c *Client) GetPosterImage(ctx context.Context, filePath string) ([]byte, string, error) {
	movie, err := c.getMovieByFilePath(ctx, filePath)
	if err != nil {
		return nil, "", fmt.Errorf("get movie for poster: %w", err)
	}

	full, err := c.radarr.GetMovieByIDContext(ctx, movie.GetID())
	if err != nil {
		return nil, "", fmt.Errorf("get movie details for poster: %w", err)
	}

	return artwork.FetchPosterImage(ctx, full.Images, c.cfg.URL, c.cfg.APIKey)
}

// GetInfo implements medialib.ArrLibrary. It returns structured metadata for
// the movie at path.
func (c *Client) GetInfo(ctx context.Context, filePath string) (medialib.MediaInfo, error) {
	return c.getMovieByFilePath(ctx, filePath)
}

// ImportByFilePath implements medialib.ArrLibrary. It sends a DownloadedMoviesScan command
// for filePath, causing Radarr to import the file at filePath into the library, and blocks
// until Radarr reports the command has reached a terminal status.
// Blocking is required so the caller can safely operate on filePath (e.g. prune the
// containing directory) once Radarr has finished moving the file into its library.
// expectedSize, when positive, is the size of the local source file in bytes
// and enables a post-check that distinguishes a benign race (Radarr's own
// completed-download handler imported the file first) from a real failure
// (Radarr kept a different pre-existing file) — see movieFileSize.
func (c *Client) ImportByFilePath(ctx context.Context, filePath string, expectedSize int64) error {
	requestPayload := struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{
		Name: "DownloadedMoviesScan",
		Path: filePath,
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(requestPayload); err != nil {
		return fmt.Errorf("encode command: %w", err)
	}

	var resp radarrlib.CommandResponse
	if err := c.radarr.PostInto(ctx, starr.Request{URI: radarrlib.APIver + "/command", Body: &body}, &resp); err != nil {
		return fmt.Errorf("scan downloaded movies at %q: %w", filePath, err)
	}

	if err := arrcommand.Wait(ctx, c.fetchCommandStatus, resp.ID, c.cfg.CommandPollInterval, "radarr"); err != nil {
		if expectedSize > 0 && errors.Is(err, arrcommand.ErrNoSuccessfulImports) {
			if size, ok := c.movieFileSize(ctx, filePath); !ok {
				slog.WarnContext(ctx, "radarr scan reported no imports and movie file lookup failed; cannot recover",
					"file_path", filePath, "expected_size", expectedSize)
			} else if size != expectedSize {
				return fmt.Errorf("radarr already has a file of size %d for %q but expected %d: %w",
					size, filePath, expectedSize, medialib.ErrLibraryFileMismatch)
			} else {
				slog.InfoContext(ctx, "radarr scan reported no imports but movie file size matches local output; treating as success",
					"file_path", filePath, "size", size)

				return nil
			}
		}

		return fmt.Errorf("wait for command for %q: %w", filePath, err)
	}

	return nil
}

// movieFileSize returns the size in bytes of the file Radarr currently has
// for the movie identified by filePath. The second return is false when
// the movie cannot be parsed, has no file, or any lookup fails — callers
// should treat that as "do not recover" so the original scan error remains
// in force.
func (c *Client) movieFileSize(ctx context.Context, filePath string) (int64, bool) {
	parsed, err := c.parseFilePath(ctx, filePath)
	if err != nil || parsed == nil {
		return 0, false
	}

	full, err := c.radarr.GetMovieByIDContext(ctx, parsed.ID)
	if err != nil || full == nil || !full.HasFile || full.MovieFile == nil {
		return 0, false
	}

	return full.MovieFile.Size, true
}

// fetchCommandStatus implements arrcommand.Fetcher. It hits Radarr's
// /command/{id} endpoint with a custom struct so the Result field
// (not exposed by starr's typed response) is decoded too.
func (c *Client) fetchCommandStatus(ctx context.Context, id int64) (arrcommand.Status, error) {
	var resp struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  string `json:"result"`
	}

	uri := path.Join(radarrlib.APIver, "command", strconv.FormatInt(id, 10))
	if err := c.radarr.GetInto(ctx, starr.Request{URI: uri}, &resp); err != nil {
		return arrcommand.Status{}, err
	}

	return arrcommand.Status{Status: resp.Status, Message: resp.Message, Result: resp.Result}, nil
}
