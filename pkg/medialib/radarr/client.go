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

// getMovieByTitle resolves a movie from a release title string — a file-name
// stem or a release folder name — via Radarr's /parse endpoint, which parses
// the string as a release name and matches it to a movie in the library. This
// works for files that have not been imported yet. Radarr's /parse accepts only
// a title (it has no path variant, unlike Sonarr's). Returns
// medialib.ErrNotFound when no movie is identified.
func (c *Client) getMovieByTitle(ctx context.Context, title string) (*medialib.Movie, error) {
	var output struct {
		Movie *radarrlib.Movie `json:"movie"`
	}

	q := make(url.Values)
	q.Set("title", title)

	req := starr.Request{URI: radarrlib.APIver + "/parse", Query: q}
	if err := c.radarr.GetInto(ctx, req, &output); err != nil {
		return nil, fmt.Errorf("get movie by title %q: %w", title, err)
	}

	if output.Movie == nil {
		return nil, medialib.ErrNotFound
	}

	return medialib.NewMovie(output.Movie.ID, output.Movie.Title, output.Movie.Year), nil
}

// getMovieByFilePath identifies the movie for a file path. It tries the file
// name first, then falls back to the containing folder name — release titles
// usually live in the folder while the file itself is often an unparseable
// hash. Works for pre-import files. Returns medialib.ErrNotFound if neither
// identifies a movie.
func (c *Client) getMovieByFilePath(ctx context.Context, filePath string) (*medialib.Movie, error) {
	base := filepath.Base(filePath)

	movie, err := c.getMovieByTitle(ctx, strings.TrimSuffix(base, filepath.Ext(base)))
	if err == nil || !errors.Is(err, medialib.ErrNotFound) {
		return movie, err
	}

	// The file name didn't identify the movie (e.g. a release hash); fall back
	// to the release folder name, left intact (no extension stripping).
	return c.getMovieByTitle(ctx, filepath.Base(filepath.Dir(filePath)))
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

// findTrackedDownloadID returns the download client's native ID for the pending
// tracked download that corresponds to the movie at filePath, or "" if none is
// found. An unidentifiable movie and API errors are both treated as "no match"
// so callers always get a safe fallback.
func (c *Client) findTrackedDownloadID(ctx context.Context, filePath string) string {
	movie, err := c.getMovieByFilePath(ctx, filePath)
	if err != nil {
		return ""
	}

	const pageSize = 100

	for page, fetched := 1, 0; ; page++ {
		curr, err := c.radarr.GetQueuePageContext(ctx, &starr.PageReq{PageSize: pageSize, Page: page})
		if err != nil {
			return ""
		}

		for _, record := range curr.Records {
			if record.MovieID == 0 || record.MovieID != movie.GetID() {
				continue
			}

			// Skip records already marked imported — Radarr completed this on its own.
			if strings.EqualFold(record.TrackedDownloadState, "imported") {
				continue
			}

			if record.DownloadID == "" {
				continue
			}

			return record.DownloadID
		}

		fetched += len(curr.Records)
		if fetched >= curr.TotalRecords || len(curr.Records) == 0 {
			break
		}
	}

	return ""
}

// ImportByFilePath implements medialib.ArrLibrary. It sends a DownloadedMoviesScan command
// for filePath, causing Radarr to import the file at filePath into the library, and blocks
// until Radarr reports the command has reached a terminal status.
// Blocking is required so the caller can safely operate on filePath (e.g. prune the
// containing directory) once Radarr has finished moving the file into its library.
// When a pending tracked download for the movie is found in the queue, its download
// client ID is included so Radarr associates the import with the tracked download —
// letting it identify the movie even when the file name is an unparseable hash — and
// calls VerifyImport to clear the download from the queue.
// expectedSize, when positive, is the size of the local source file in bytes
// and enables a post-check that distinguishes a benign race (Radarr's own
// completed-download handler imported the file first) from a real failure
// (Radarr kept a different pre-existing file) — see movieFileSize.
func (c *Client) ImportByFilePath(ctx context.Context, filePath string, expectedSize int64) error {
	downloadClientID := c.findTrackedDownloadID(ctx, filePath)

	requestPayload := struct {
		Name             string `json:"name"`
		Path             string `json:"path"`
		DownloadClientId string `json:"downloadClientId,omitempty"`
	}{
		Name:             "DownloadedMoviesScan",
		Path:             filePath,
		DownloadClientId: downloadClientID,
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
		if errors.Is(err, arrcommand.ErrNoSuccessfulImports) {
			// The scan imported nothing. Before treating that as a failure to
			// retry, check whether the movie is still in the library: if Radarr
			// can no longer match the file to a library movie (the movie was
			// removed or is no longer monitored), the import can never succeed.
			// Surface medialib.ErrNotFound so the caller treats this as a benign
			// skip rather than burning the full retry budget on it.
			if _, lookupErr := c.getMovieByFilePath(ctx, filePath); errors.Is(lookupErr, medialib.ErrNotFound) {
				slog.InfoContext(ctx, "radarr scan reported no imports and movie is no longer in the library; skipping import",
					"file_path", filePath)

				return fmt.Errorf("import %q: %w", filePath, medialib.ErrNotFound)
			}

			// The movie is still in the library, so fall through to the
			// benign-race size post-check documented on expectedSize above.
			if expectedSize > 0 {
				if size, ok := c.movieFileSize(ctx, filePath); ok && size == expectedSize {
					slog.InfoContext(ctx, "radarr scan reported no imports but movie file size matches local output; treating as success",
						"file_path", filePath, "size", size)

					return nil
				}
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
	// getMovieByFilePath falls back to the containing folder name when the file
	// name is an unparseable hash; without that the race post-check could not
	// identify the movie and a benign race (Radarr imported our transcode
	// first) would surface as a spurious failure.
	movie, err := c.getMovieByFilePath(ctx, filePath)
	if err != nil {
		return 0, false
	}

	full, err := c.radarr.GetMovieByIDContext(ctx, movie.GetID())
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
