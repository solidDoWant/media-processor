// Package sonarr provides a Sonarr client implementing medialib.ArrLibrary.
package sonarr

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
	sonarrlib "golift.io/starr/sonarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/arrcommand"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/artwork"
	"github.com/solidDoWant/media-processor/pkg/medialib/internal/trackeddownload"
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

// parseEpisode identifies the episode for a release title (a file name stem
// or a release folder name) with Sonarr's /parse endpoint, which works for
// files that haven't been imported yet. We pass a title rather than a path
// because /parse only matches paths inside the library.
//
// The error wraps medialib.ErrNotFound when no episode is identified. It also
// wraps trackeddownload.ErrUnparseable when Sonarr couldn't parse the title as
// a release at all (e.g. an obfuscated name), as opposed to parsing it but
// finding no matching series or episode in the library.
func (c *Client) parseEpisode(ctx context.Context, title string) (*medialib.Episode, error) {
	parsed, err := c.sonarr.ParseContext(ctx, &sonarrlib.ParseInput{Title: title})
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", title, err)
	}

	if parsed == nil || parsed.ParsedEpisodeInfo == nil {
		return nil, fmt.Errorf("parse %q: %w: %w", title, medialib.ErrNotFound, trackeddownload.ErrUnparseable)
	}

	if len(parsed.Episodes) == 0 {
		return nil, fmt.Errorf("parse %q: %w", title, medialib.ErrNotFound)
	}

	ep := parsed.Episodes[0]

	var year int
	if parsed.ParsedEpisodeInfo.SeriesTitleInfo != nil {
		year = parsed.ParsedEpisodeInfo.SeriesTitleInfo.Year
	}

	return medialib.NewEpisode(ep.ID, ep.SeriesID, ep.Title, parsed.Title, year, ep.SeasonNumber, ep.EpisodeNumber), nil
}

// getEpisodeByFilePath identifies the episode for a file path. It tries the
// file name first, then the folder name, because the release title usually
// lives in the folder while the file itself may have an obfuscated name.
//
// The error wraps medialib.ErrNotFound when neither identifies an episode. It
// also wraps trackeddownload.ErrUnparseable when neither name could be parsed
// as a release, so callers can tell "unknown file" apart from "not in the
// library".
func (c *Client) getEpisodeByFilePath(ctx context.Context, filePath string) (*medialib.Episode, error) {
	base := filepath.Base(filePath)

	episode, err := c.parseEpisode(ctx, strings.TrimSuffix(base, filepath.Ext(base)))
	if !errors.Is(err, medialib.ErrNotFound) {
		return episode, err
	}

	episode, folderErr := c.parseEpisode(ctx, filepath.Base(filepath.Dir(filePath)))
	if !errors.Is(folderErr, medialib.ErrNotFound) {
		return episode, folderErr
	}

	// Neither name identified an episode. If the file name parsed as a
	// release, report its "not in the library" result rather than the
	// folder's, which may only be unparseable.
	if !errors.Is(err, trackeddownload.ErrUnparseable) {
		return nil, err
	}

	return nil, folderErr
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

// findTrackedDownload returns the queue entry for the download that filePath
// came from. It matches on the queue entry's output folder first (see package
// trackeddownload), then falls back to the episode identified by parsing the
// file and folder names. API errors are treated as "no match".
func (c *Client) findTrackedDownload(ctx context.Context, filePath string) (trackeddownload.Download, bool) {
	const pageSize = 100

	var records []trackeddownload.Record

	for page, fetched := 1, 0; ; page++ {
		curr, err := c.sonarr.GetQueuePageContext(ctx, &starr.PageReq{PageSize: pageSize, Page: page})
		if err != nil {
			return trackeddownload.Download{}, false
		}

		for _, record := range curr.Records {
			records = append(records, trackeddownload.Record{
				QueueID:    record.ID,
				ItemID:     record.EpisodeID,
				DownloadID: record.DownloadID,
				OutputPath: record.OutputPath,
				State:      record.TrackedDownloadState,
			})
		}

		fetched += len(curr.Records)
		if fetched >= curr.TotalRecords || len(curr.Records) == 0 {
			break
		}
	}

	if download, ok := trackeddownload.MatchPath(records, filePath); ok {
		return download, true
	}

	episode, err := c.getEpisodeByFilePath(ctx, filePath)
	if err != nil {
		return trackeddownload.Download{}, false
	}

	return trackeddownload.MatchItem(records, episode.GetID())
}

// removeFromQueue removes a tracked download from Sonarr's queue after we
// decided not to import it. Without this the queue entry stays forever: its
// files are deleted by cleanup, so Sonarr keeps reporting "No files found are
// eligible for import". The download client's copy is left alone and the
// release is not blocklisted. Downloads covering several episodes (season
// packs) are left in place, because removing one removes them all.
// Errors are logged, not returned: the import decision is already made.
func (c *Client) removeFromQueue(ctx context.Context, download trackeddownload.Download, filePath string) {
	if download.Records != 1 {
		slog.InfoContext(ctx, "not removing skipped download from the sonarr queue because it covers several episodes",
			"file_path", filePath, "download_id", download.DownloadID, "queue_records", download.Records)

		return
	}

	opts := &starr.QueueDeleteOpts{RemoveFromClient: starr.False(), SkipRedownload: true}
	if err := c.sonarr.DeleteQueueContext(ctx, download.QueueID, opts); err != nil {
		slog.WarnContext(ctx, "failed to remove skipped download from the sonarr queue",
			"file_path", filePath, "download_id", download.DownloadID, "error", err)

		return
	}

	slog.InfoContext(ctx, "removed skipped download from the sonarr queue",
		"file_path", filePath, "download_id", download.DownloadID)
}

// ImportByFilePath implements medialib.ArrLibrary. It sends a DownloadedEpisodesScan command
// for filePath, causing Sonarr to import the file at filePath into the library, and blocks
// until Sonarr reports the command has reached a terminal status.
// Blocking is required so the caller can safely operate on filePath (e.g. prune the
// containing directory) once Sonarr has finished moving the file into its library.
// When the queue entry for the download is found (see findTrackedDownload),
// its download client ID is included. Sonarr then knows which episode the file
// is without parsing its name, and calls VerifyImport to clear the queue entry.
// expectedSize, when positive, is the size of the local source file in bytes
// and enables a post-check that distinguishes a benign race (Sonarr's own
// completed-download handler imported the file first) from a real failure
// (Sonarr kept a different pre-existing file) — see episodeFileSize.
func (c *Client) ImportByFilePath(ctx context.Context, filePath string, expectedSize int64) error {
	download, tracked := c.findTrackedDownload(ctx, filePath)
	downloadClientID := download.DownloadID

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
		if errors.Is(err, arrcommand.ErrNoSuccessfulImports) {
			// The scan imported nothing. Check whether the import can never
			// succeed, so the caller skips it instead of retrying.
			//
			// "Not in the library" is only possible when Sonarr has no queue
			// entry for the download (a queue entry always points at a library
			// episode), and only when Sonarr parsed the release name but found
			// no matching series. A name it couldn't parse at all says nothing
			// about the library, so that case is retried and eventually fails
			// loudly with the output left in place.
			if !tracked {
				if _, lookupErr := c.getEpisodeByFilePath(ctx, filePath); errors.Is(lookupErr, medialib.ErrNotFound) &&
					!errors.Is(lookupErr, trackeddownload.ErrUnparseable) {
					slog.InfoContext(ctx, "sonarr scan reported no imports and the release matches no series in the library; skipping import",
						"file_path", filePath)

					return fmt.Errorf("import %q: %w", filePath, medialib.ErrNotFound)
				}
			}

			// If Sonarr rejected the import because its existing file is
			// already as good or better ("not an upgrade"), the scan can never
			// succeed.
			if c.rejectedAsNotUpgrade(ctx, filePath, downloadClientID) {
				slog.InfoContext(ctx, "sonarr scan reported no imports because the existing episode file is already an equal or better version; skipping import",
					"file_path", filePath)

				if tracked {
					c.removeFromQueue(ctx, download, filePath)
				}

				return fmt.Errorf("import %q: %w", filePath, medialib.ErrNotUpgrade)
			}

			// Otherwise fall through to the benign-race size post-check
			// documented on expectedSize above.
			if expectedSize > 0 {
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

// rejectedAsNotUpgrade reports whether importing filePath is permanently
// rejected because the library already holds an equal or better file ("not an
// upgrade" / "not a Custom Format upgrade"). It asks Sonarr's manual-import
// endpoint to evaluate the file fresh, so the rejection is detected whether or
// not a tracked download still exists in the queue — the queue record is gone
// once Sonarr has finished processing a competing release for the same episode.
//
// downloadClientID, when set, is forwarded so Sonarr can associate the file
// with its tracked download. API errors and an output that does not contain a
// matching, rejected entry return false so the caller falls back to its normal
// failure handling.
func (c *Client) rejectedAsNotUpgrade(ctx context.Context, filePath, downloadClientID string) bool {
	query := make(url.Values)
	query.Set("folder", filepath.Dir(filePath))
	query.Set("filterExistingFiles", "false")

	if downloadClientID != "" {
		query.Set("downloadId", downloadClientID)
	}

	// Sonarr's GET /manualimport returns an array; starr's typed helper decodes
	// it into a single value, so request it raw into a slice instead.
	var items []struct {
		Path       string `json:"path"`
		Rejections []struct {
			Reason string `json:"reason"`
		} `json:"rejections"`
	}

	req := starr.Request{URI: sonarrlib.APIver + "/manualimport", Query: query}
	if err := c.sonarr.GetInto(ctx, req, &items); err != nil {
		return false
	}

	base := filepath.Base(filePath)

	for _, item := range items {
		if filepath.Base(item.Path) != base {
			continue
		}

		for _, rejection := range item.Rejections {
			if arrcommand.IsNotUpgradeRejection(rejection.Reason) {
				return true
			}
		}
	}

	return false
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
