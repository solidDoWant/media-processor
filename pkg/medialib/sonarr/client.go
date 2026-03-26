// Package sonarr provides a Sonarr client implementing medialib.EpisodeLibrary.
package sonarr

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"golift.io/starr"
	sonarrlib "golift.io/starr/sonarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// Compile-time assertions that *Client implements medialib.EpisodeLibrary and medialib.LibraryClient.
var _ medialib.EpisodeLibrary = (*Client)(nil)
var _ medialib.Library = (*Client)(nil)

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

// GetEpisodeByFilePath returns the episode identified by parsing the file path.
// Uses Sonarr's parse endpoint, so it works for pre-import files.
// Returns medialib.ErrNotFound if no episode is identified.
func (c *Client) GetEpisodeByFilePath(ctx context.Context, path string) (medialib.Episode, error) {
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
			return medialib.Episode{}, fmt.Errorf("path %q is outside configured remote prefix %q", path, cleanPrefix)
		}
	}

	parsed, err := c.sonarr.ParseContext(ctx, &sonarrlib.ParseInput{Path: path})
	if err != nil {
		return medialib.Episode{}, fmt.Errorf("parse file path: %w", err)
	}

	if parsed == nil || parsed.ParsedEpisodeInfo == nil || len(parsed.Episodes) == 0 {
		return medialib.Episode{}, medialib.ErrNotFound
	}

	ep := parsed.Episodes[0]
	return medialib.Episode{
		ID:            ep.ID,
		SeriesID:      ep.SeriesID,
		SeriesTitle:   parsed.Title,
		SeasonNumber:  ep.SeasonNumber,
		EpisodeNumber: ep.EpisodeNumber,
	}, nil
}

// GetIDByFilePath implements medialib.LibraryClient. It looks up the episode
// by file path and returns the series ID (used for library refresh).
func (c *Client) GetIDByFilePath(ctx context.Context, path string) (int64, error) {
	episode, err := c.GetEpisodeByFilePath(ctx, path)
	if err != nil {
		return 0, err
	}
	return episode.SeriesID, nil
}

// Refresh implements medialib.LibraryClient by triggering a Sonarr library
// rescan for the given series ID.
func (c *Client) Refresh(ctx context.Context, id int64) error {
	return c.RefreshSeries(ctx, id)
}

// RefreshSeries triggers a Sonarr library rescan for the given series ID.
func (c *Client) RefreshSeries(ctx context.Context, seriesID int64) error {
	_, err := c.sonarr.SendCommandContext(ctx, &sonarrlib.CommandRequest{
		Name:      "RefreshSeries",
		SeriesIDs: []int64{seriesID},
	})
	if err != nil {
		return fmt.Errorf("refresh series %d: %w", seriesID, err)
	}

	return nil
}
