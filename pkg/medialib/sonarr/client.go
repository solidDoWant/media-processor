// Package sonarr provides a Sonarr client implementing medialib.EpisodeLibrary.
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

// Compile-time assertions that *Client implements medialib.EpisodeLibrary and medialib.ArrLibrary.
var _ medialib.EpisodeLibrary = (*Client)(nil)
var _ medialib.ArrLibrary = (*Client)(nil)

// Config holds the configuration for a Sonarr client.
type Config struct {
	URL    string
	APIKey string
	// LocalPathPrefix and RemotePathPrefix enable optional path translation.
	// When set, paths starting with LocalPathPrefix are rewritten to use
	// RemotePathPrefix before being matched against Sonarr's stored paths.
	// This handles cases where the worker and Sonarr use different mount points
	// for the same storage.
	LocalPathPrefix  string
	RemotePathPrefix string
}

// Client is a Sonarr client implementing medialib.EpisodeLibrary.
type Client struct {
	cfg    Config
	sonarr *sonarrlib.Sonarr
}

// New creates a new Sonarr client.
func New(cfg Config) *Client {
	s := starr.New(cfg.APIKey, cfg.URL, 0)
	return &Client{cfg: cfg, sonarr: sonarrlib.New(s)}
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

// GetEpisodeByFilePath returns the episode identified by parsing the file path.
// Uses Sonarr's parse endpoint, so it works for pre-import files.
// Returns medialib.ErrNotFound if no episode is identified.
func (c *Client) GetEpisodeByFilePath(ctx context.Context, path string) (medialib.Episode, error) {
	var err error

	path, err = c.translatePath(path)
	if err != nil {
		return medialib.Episode{}, err
	}

	parsed, err := c.sonarr.ParseContext(ctx, &sonarrlib.ParseInput{Path: path})
	if err != nil {
		return medialib.Episode{}, fmt.Errorf("parse file path: %w", err)
	}

	if parsed == nil || parsed.ParsedEpisodeInfo == nil || len(parsed.Episodes) == 0 {
		return medialib.Episode{}, medialib.ErrNotFound
	}

	ep := parsed.Episodes[0]

	var year int
	if parsed.ParsedEpisodeInfo.SeriesTitleInfo != nil {
		year = parsed.ParsedEpisodeInfo.SeriesTitleInfo.Year
	}

	return medialib.Episode{
		ID:            ep.ID,
		SeriesID:      ep.SeriesID,
		Title:         ep.Title,
		Year:          year,
		SeriesTitle:   parsed.Title,
		SeasonNumber:  ep.SeasonNumber,
		EpisodeNumber: ep.EpisodeNumber,
	}, nil
}

// GetPosterImage implements medialib.ArrLibrary. It returns the raw poster
// image bytes and MIME type for the series containing the episode at path.
// Returns nil bytes (no error) when no JPEG or PNG poster is available.
// Returns medialib.ErrNotFound if the path cannot be matched to an episode.
// Returns other errors if the library is unreachable or a Sonarr API call fails.
func (c *Client) GetPosterImage(ctx context.Context, path string) ([]byte, string, error) {
	episode, err := c.GetEpisodeByFilePath(ctx, path)
	if err != nil {
		return nil, "", fmt.Errorf("get episode for poster: %w", err)
	}

	series, err := c.sonarr.GetSeriesByIDContext(ctx, episode.SeriesID)
	if err != nil {
		return nil, "", fmt.Errorf("get series details for poster: %w", err)
	}

	return artwork.FetchPosterImage(ctx, series.Images, c.cfg.URL, c.cfg.APIKey)
}

// GetInfo implements medialib.ArrLibrary. It returns structured metadata for
// the episode at path.
func (c *Client) GetInfo(ctx context.Context, path string) (medialib.MediaInfo, error) {
	episode, err := c.GetEpisodeByFilePath(ctx, path)
	if err != nil {
		return nil, err
	}

	return &episode, nil
}

// ImportByFilePath implements medialib.ArrLibrary. It translates path to
// Sonarr's view and sends a DownloadedEpisodesScan command for that path,
// causing Sonarr to import the file at path into the library.
func (c *Client) ImportByFilePath(ctx context.Context, path string) error {
	translated, err := c.translatePath(path)
	if err != nil {
		return err
	}

	requestPayload := struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{
		Name: "DownloadedEpisodesScan",
		Path: translated,
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(requestPayload); err != nil {
		return fmt.Errorf("encode command: %w", err)
	}

	var resp sonarrlib.CommandResponse

	if err := c.sonarr.PostInto(ctx, starr.Request{URI: sonarrlib.APIver + "/command", Body: &body}, &resp); err != nil {
		return fmt.Errorf("scan downloaded episodes at %q: %w", translated, err)
	}

	return nil
}
