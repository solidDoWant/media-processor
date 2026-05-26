// Package sonarr provides a Sonarr client implementing medialib.ArrLibrary.
package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golift.io/starr"
	sonarrlib "golift.io/starr/sonarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/arrcommand"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/artwork"
)

// Compile-time assertion that *Client implements medialib.ArrLibrary.
var _ medialib.ArrLibrary = (*Client)(nil)

// Config holds the configuration for a Sonarr client.
type Config struct {
	URL    string
	APIKey string
	// CommandPollInterval overrides the default poll interval for waiting on
	// command completion. Zero uses arrcommand.DefaultPollInterval.
	CommandPollInterval time.Duration
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
func (c *Client) getEpisodeByFilePath(ctx context.Context, filePath string) (*medialib.Episode, error) {
	// Use the title parameter (filename stem) rather than path because Sonarr's
	// parse endpoint matches path against library paths only, returning 204 No
	// Content for download paths that haven't been imported yet.
	base := filepath.Base(filePath)

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
func (c *Client) GetPosterImage(ctx context.Context, filePath string) ([]byte, string, error) {
	episode, err := c.getEpisodeByFilePath(ctx, filePath)
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
func (c *Client) GetInfo(ctx context.Context, filePath string) (medialib.MediaInfo, error) {
	return c.getEpisodeByFilePath(ctx, filePath)
}

// findTrackedDownloadID returns the download client's native ID for the pending
// tracked download that corresponds to the episode at path, or "" if none is
// found. API errors are treated as "no match" so callers always get a safe
// fallback.
func (c *Client) findTrackedDownloadID(ctx context.Context, filePath string) string {
	episode, err := c.getEpisodeByFilePath(ctx, filePath)
	if err != nil {
		return ""
	}

	const pageSize = 100

	for page, fetched := 1, 0; ; page++ {
		curr, err := c.sonarr.GetQueuePageContext(ctx, &starr.PageReq{PageSize: pageSize, Page: page})
		if err != nil {
			return ""
		}

		for _, record := range curr.Records {
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

		fetched += len(curr.Records)
		if fetched >= curr.TotalRecords || len(curr.Records) == 0 {
			break
		}
	}

	return ""
}

// ImportByFilePath implements medialib.ArrLibrary. It sends a DownloadedEpisodesScan command
// for filePath, causing Sonarr to import the file at filePath into the library, and blocks
// until Sonarr reports the command has reached a terminal status.
// Blocking is required so the caller can safely operate on filePath (e.g. prune the
// containing directory) once Sonarr has finished moving the file into its library.
// When a pending tracked download for the episode is found in the queue, its
// download client ID is included so Sonarr calls VerifyImport and removes the
// item from the downloads queue.
// expectedSize, when positive, is the size of the local source file in bytes
// and enables a post-check that distinguishes a benign race (Sonarr's own
// completed-download handler imported the file first) from a real failure
// (Sonarr kept a different pre-existing file) — see episodeFileSize.
func (c *Client) ImportByFilePath(ctx context.Context, filePath string, expectedSize int64) error {
	downloadClientID := c.findTrackedDownloadID(ctx, filePath)

	requestPayload := struct {
		Name             string `json:"name"`
		Path             string `json:"path"`
		DownloadClientId string `json:"downloadClientId,omitempty"`
	}{
		Name:             "DownloadedEpisodesScan",
		Path:             filePath,
		DownloadClientId: downloadClientID,
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(requestPayload); err != nil {
		return fmt.Errorf("encode command: %w", err)
	}

	var resp sonarrlib.CommandResponse

	if err := c.sonarr.PostInto(ctx, starr.Request{URI: sonarrlib.APIver + "/command", Body: &body}, &resp); err != nil {
		return fmt.Errorf("scan downloaded episodes at %q: %w", filePath, err)
	}

	if err := arrcommand.Wait(ctx, c.fetchCommandStatus, resp.ID, c.cfg.CommandPollInterval, "sonarr"); err != nil {
		if expectedSize > 0 && errors.Is(err, arrcommand.ErrNoSuccessfulImports) {
			if size, ok := c.episodeFileSize(ctx, filePath); !ok {
				slog.WarnContext(ctx, "sonarr scan reported no imports and episode file lookup failed; cannot recover",
					"file_path", filePath, "expected_size", expectedSize)
			} else if size != expectedSize {
				slog.WarnContext(ctx, "sonarr scan reported no imports and episode file size does not match expected; cannot recover",
					"file_path", filePath, "sonarr_size", size, "expected_size", expectedSize)
			} else {
				slog.InfoContext(ctx, "sonarr scan reported no imports but episode file size matches local output; treating as success",
					"file_path", filePath, "size", size)

				return nil
			}
		}

		return fmt.Errorf("wait for command for %q: %w", filePath, err)
	}

	return nil
}

// episodeFileSize returns the size in bytes of the file Sonarr currently has
// for the episode identified by filePath. The second return is false when
// the episode cannot be parsed, has no file, or any lookup fails — callers
// should treat that as "do not recover" so the original scan error remains
// in force.
func (c *Client) episodeFileSize(ctx context.Context, filePath string) (int64, bool) {
	ep, err := c.getEpisodeByFilePath(ctx, filePath)
	if err != nil {
		return 0, false
	}

	full, err := c.sonarr.GetEpisodeByIDContext(ctx, ep.GetID())
	if err != nil || full == nil || !full.HasFile || full.EpisodeFileID == 0 {
		return 0, false
	}

	files, err := c.sonarr.GetEpisodeFilesContext(ctx, full.EpisodeFileID)
	if err != nil || len(files) == 0 || files[0] == nil {
		return 0, false
	}

	return files[0].Size, true
}

// fetchCommandStatus implements arrcommand.Fetcher. It hits Sonarr's
// /command/{id} endpoint with a custom struct so the Result field
// (not exposed by starr's typed response) is decoded too.
func (c *Client) fetchCommandStatus(ctx context.Context, id int64) (arrcommand.Status, error) {
	var resp struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  string `json:"result"`
	}

	uri := path.Join(sonarrlib.APIver, "command", strconv.FormatInt(id, 10))
	if err := c.sonarr.GetInto(ctx, starr.Request{URI: uri}, &resp); err != nil {
		return arrcommand.Status{}, err
	}

	return arrcommand.Status{Status: resp.Status, Message: resp.Message, Result: resp.Result}, nil
}
