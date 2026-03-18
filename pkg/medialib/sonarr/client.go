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

// Compile-time assertion that *Client implements medialib.EpisodeLibrary.
var _ medialib.EpisodeLibrary = (*Client)(nil)

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

// GetEpisodeByFilePath returns the episode whose file matches path.
// Returns medialib.ErrNotFound if no match is found.
//
// The implementation first fetches all series and uses each series' root path
// as a prefix filter, so episode files are fetched for only the one series
// whose root path matches — reducing API calls from O(N series) to O(1).
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

	series, err := c.sonarr.GetAllSeriesContext(ctx)
	if err != nil {
		return medialib.Episode{}, fmt.Errorf("list series: %w", err)
	}

	// Use each series' root path as a prefix filter to avoid fetching episode
	// files for every series in the library.
	for _, s := range series {
		if !strings.HasPrefix(path, filepath.Clean(s.Path)+string(filepath.Separator)) {
			continue
		}

		files, err := c.sonarr.GetSeriesEpisodeFilesContext(ctx, s.ID)
		if err != nil {
			return medialib.Episode{}, fmt.Errorf("list episode files for series %d: %w", s.ID, err)
		}

		for _, file := range files {
			if file.Path != path {
				continue
			}

			episodes, err := c.sonarr.GetSeriesEpisodesContext(ctx, &sonarrlib.GetEpisode{EpisodeFileID: file.ID})
			if err != nil {
				return medialib.Episode{}, fmt.Errorf("get episode for file %d: %w", file.ID, err)
			}

			if len(episodes) == 0 {
				return medialib.Episode{}, fmt.Errorf("no episode found for file %d: %w", file.ID, medialib.ErrNotFound)
			}

			ep := episodes[0]
			return medialib.Episode{
				ID:            ep.ID,
				SeriesTitle:   s.Title,
				SeasonNumber:  ep.SeasonNumber,
				EpisodeNumber: ep.EpisodeNumber,
			}, nil
		}
	}

	return medialib.Episode{}, medialib.ErrNotFound
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
