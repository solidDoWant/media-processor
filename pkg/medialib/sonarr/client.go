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

// GetInfo implements medialib.ArrLibrary. It returns structured metadata for
// the episode at path.
func (c *Client) GetInfo(ctx context.Context, path string) (medialib.MediaInfo, error) {
	episode, err := c.GetEpisodeByFilePath(ctx, path)
	if err != nil {
		return nil, err
	}
	return &episode, nil
}

// RefreshByFilePath implements medialib.ArrLibrary. It looks up the episode by
// file path and triggers a Sonarr series rescan. Sonarr only supports
// series-level refresh, so the series ID (not the episode ID) is used.
func (c *Client) RefreshByFilePath(ctx context.Context, path string) error {
	episode, err := c.GetEpisodeByFilePath(ctx, path)
	if err != nil {
		return err
	}

	_, err = c.sonarr.SendCommandContext(ctx, &sonarrlib.CommandRequest{
		Name:      "RefreshSeries",
		SeriesIDs: []int64{episode.SeriesID},
	})
	if err != nil {
		return fmt.Errorf("refresh series %d: %w", episode.SeriesID, err)
	}

	return nil
}
