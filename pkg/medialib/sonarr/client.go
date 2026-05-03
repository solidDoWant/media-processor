// Package sonarr provides a Sonarr client implementing medialib.ArrLibrary.
package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"golift.io/starr"
	sonarrlib "golift.io/starr/sonarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/artwork"
)

// Compile-time assertion that *Client implements medialib.ArrLibrary.
var _ medialib.ArrLibrary = (*Client)(nil)

// Config holds the configuration for a Sonarr client.
type Config struct {
	URL    string
	APIKey string
}

// Client is a Sonarr client implementing medialib.ArrLibrary.
type Client struct {
	cfg    Config
	sonarr *sonarrlib.Sonarr
}

// New creates a new Sonarr client.
func New(cfg Config) *Client {
	s := starr.New(cfg.APIKey, cfg.URL, 0)
	return &Client{cfg: cfg, sonarr: sonarrlib.New(s)}
}

// getEpisodeByFilePath returns the episode identified by parsing the file path.
// Uses Sonarr's parse endpoint, so it works for pre-import files.
// Returns medialib.ErrNotFound if no episode is identified.
func (c *Client) getEpisodeByFilePath(ctx context.Context, path string) (*medialib.Episode, error) {
	// Use the title parameter (filename stem) rather than path because Sonarr's
	// parse endpoint matches path against library paths only, returning 204 No
	// Content for download paths that haven't been imported yet.
	base := filepath.Base(path)

	parsed, err := c.sonarr.ParseContext(ctx, &sonarrlib.ParseInput{Title: strings.TrimSuffix(base, filepath.Ext(base))})
	if err != nil {
		return nil, fmt.Errorf("parse file path: %w", err)
	}

	if parsed == nil || parsed.ParsedEpisodeInfo == nil || len(parsed.Episodes) == 0 {
		return nil, medialib.ErrNotFound
	}

	ep := parsed.Episodes[0]

	var year int
	if parsed.ParsedEpisodeInfo.SeriesTitleInfo != nil {
		year = parsed.ParsedEpisodeInfo.SeriesTitleInfo.Year
	}

	return medialib.NewEpisode(ep.ID, ep.SeriesID, ep.Title, parsed.Title, year, ep.SeasonNumber, ep.EpisodeNumber), nil
}

// GetPosterImage implements medialib.ArrLibrary. It returns the raw poster
// image bytes and MIME type for the series containing the episode at path.
// Returns nil bytes (no error) when no JPEG or PNG poster is available.
// Returns medialib.ErrNotFound if the path cannot be matched to an episode.
// Returns other errors if the library is unreachable or a Sonarr API call fails.
func (c *Client) GetPosterImage(ctx context.Context, path string) ([]byte, string, error) {
	episode, err := c.getEpisodeByFilePath(ctx, path)
	if err != nil {
		return nil, "", fmt.Errorf("get episode for poster: %w", err)
	}

	series, err := c.sonarr.GetSeriesByIDContext(ctx, episode.SeriesID())
	if err != nil {
		return nil, "", fmt.Errorf("get series details for poster: %w", err)
	}

	return artwork.FetchPosterImage(ctx, series.Images, c.cfg.URL, c.cfg.APIKey)
}

// GetInfo implements medialib.ArrLibrary. It returns structured metadata for
// the episode at path.
func (c *Client) GetInfo(ctx context.Context, path string) (medialib.MediaInfo, error) {
	return c.getEpisodeByFilePath(ctx, path)
}

// findTrackedDownloadID returns the download client's native ID for the pending
// tracked download that corresponds to the episode at path, or "" if none is
// found. API errors are treated as "no match" so callers always get a safe
// fallback.
func (c *Client) findTrackedDownloadID(ctx context.Context, path string) string {
	episode, err := c.getEpisodeByFilePath(ctx, path)
	if err != nil {
		return ""
	}

	queue, err := c.sonarr.GetQueueContext(ctx, 0, 100)
	if err != nil {
		return ""
	}

	for _, record := range queue.Records {
		if record.EpisodeID == 0 || record.EpisodeID != episode.GetID() {
			continue
		}
		// Skip records already marked imported — Sonarr completed this on its own.
		if strings.EqualFold(record.TrackedDownloadState, "imported") {
			continue
		}
		if record.DownloadID == "" {
			continue
		}
		return record.DownloadID
	}

	return ""
}

// ImportByFilePath implements medialib.ArrLibrary. It sends a DownloadedEpisodesScan command
// for path, causing Sonarr to import the file at path into the library.
// When a pending tracked download for the episode is found in the queue, its
// download client ID is included so Sonarr calls VerifyImport and removes the
// item from the downloads queue.
func (c *Client) ImportByFilePath(ctx context.Context, path string) error {
	downloadClientID := c.findTrackedDownloadID(ctx, path)

	requestPayload := struct {
		Name             string `json:"name"`
		Path             string `json:"path"`
		DownloadClientId string `json:"downloadClientId,omitempty"`
	}{
		Name:             "DownloadedEpisodesScan",
		Path:             path,
		DownloadClientId: downloadClientID,
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(requestPayload); err != nil {
		return fmt.Errorf("encode command: %w", err)
	}

	var resp sonarrlib.CommandResponse

	if err := c.sonarr.PostInto(ctx, starr.Request{URI: sonarrlib.APIver + "/command", Body: &body}, &resp); err != nil {
		return fmt.Errorf("scan downloaded episodes at %q: %w", path, err)
	}

	return nil
}
