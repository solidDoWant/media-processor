package sonarr_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golift.io/starr"
	sonarrlib "golift.io/starr/sonarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/sonarr"
)

// unreachableURL is a URL that always refuses connections (port 1 is a
// privileged port with no listener in any normal environment).
const unreachableURL = "http://127.0.0.1:1"

// fastPollInterval keeps tests that exercise the command polling loop snappy.
const fastPollInterval = time.Millisecond

// sonarrTestServerConfig holds configuration for test HTTP servers.
type sonarrTestServerConfig struct {
	parseResp       *sonarrlib.ParseOutput
	seriesByID      *sonarrlib.Series      // response for /api/v3/series/{id}
	episodeByID     *sonarrlib.Episode     // response for /api/v3/episode/{id}; nil → 404
	episodeFile     *sonarrlib.EpisodeFile // single file returned by /api/v3/episodeFile; nil → empty array
	queueResp       *sonarrlib.Queue       // single-page queue response; nil returns empty queue
	queuePages      []*sonarrlib.Queue     // multi-page queue responses; index 0 = page 1; takes priority over queueResp
	queueHTTPStatus int                    // override HTTP status for /api/v3/queue; 0 means 200
	imageBody       []byte
	imageType       string
	// commandStatuses is the sequence of status values returned by GET
	// /api/v3/command/{id} on successive polls. The last entry is reused for
	// any further polls. Empty defaults to a single "completed" entry.
	commandStatuses []string
	// commandResult is the value of the response's "result" field. Sonarr
	// returns "unsuccessful" when a command finishes but reports no useful
	// work (e.g. import that found nothing eligible). Empty leaves the field
	// out of the response.
	commandResult string
	// commandStatusMessage is the message attached to non-terminal/terminal
	// status responses. Useful to assert the wrapped error mentions it.
	commandStatusMessage string
	// commandStatusHTTPStatus, when non-zero, is returned by GET
	// /api/v3/command/{id} instead of the JSON body.
	commandStatusHTTPStatus int
	// onCommand is called with the raw command request when /api/v3/command is hit.
	onCommand func(t *testing.T, r *http.Request)
	// onParse is called with the raw parse request when /api/v3/parse is hit.
	onParse func(t *testing.T, r *http.Request)
}

func newSonarrTestServer(t *testing.T, parseResp *sonarrlib.ParseOutput) *httptest.Server {
	t.Helper()
	return newSonarrTestServerWithConfig(t, sonarrTestServerConfig{parseResp: parseResp})
}

func newSonarrTestServerWithConfig(t *testing.T, cfg sonarrTestServerConfig) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/parse", func(w http.ResponseWriter, r *http.Request) {
		if cfg.onParse != nil {
			cfg.onParse(t, r)
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(cfg.parseResp))
	})

	mux.HandleFunc("/api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		status := cfg.queueHTTPStatus
		if status == 0 {
			status = http.StatusOK
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)

		if status != http.StatusOK {
			return
		}

		var q *sonarrlib.Queue

		if cfg.queuePages != nil {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 1 {
				page = 1
			}

			if page-1 < len(cfg.queuePages) {
				q = cfg.queuePages[page-1]
			} else {
				q = &sonarrlib.Queue{Records: []*sonarrlib.QueueRecord{}}
			}
		} else {
			q = cfg.queueResp
			if q == nil {
				q = &sonarrlib.Queue{Records: []*sonarrlib.QueueRecord{}}
			}
		}

		require.NoError(t, json.NewEncoder(w).Encode(q))
	})

	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		if cfg.onCommand != nil {
			cfg.onCommand(t, r)
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(&sonarrlib.CommandResponse{ID: 1, Status: "queued"}))
	})

	// /api/v3/command/{id} drives the post-submit polling loop. Default
	// behavior is to immediately return a terminal "completed" so existing
	// tests don't have to opt in. Tests that want to exercise the polling
	// loop set commandStatuses to a sequence of returns.
	var commandStatusCalls atomic.Int32

	mux.HandleFunc("/api/v3/command/", func(w http.ResponseWriter, r *http.Request) {
		if cfg.commandStatusHTTPStatus != 0 {
			w.WriteHeader(cfg.commandStatusHTTPStatus)
			return
		}

		statuses := cfg.commandStatuses
		if len(statuses) == 0 {
			statuses = []string{"completed"}
		}

		idx := int(commandStatusCalls.Add(1) - 1)
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}

		// starr's typed CommandResponse doesn't carry the Result field, so
		// build the body manually to include it.
		body := map[string]any{
			"id":      1,
			"status":  statuses[idx],
			"message": cfg.commandStatusMessage,
		}

		if cfg.commandResult != "" {
			body["result"] = cfg.commandResult
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	})

	mux.HandleFunc("/api/v3/series/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(cfg.seriesByID))
	})

	mux.HandleFunc("/api/v3/episode/", func(w http.ResponseWriter, r *http.Request) {
		if cfg.episodeByID == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(cfg.episodeByID))
	})

	mux.HandleFunc("/api/v3/episodeFile", func(w http.ResponseWriter, r *http.Request) {
		var files []*sonarrlib.EpisodeFile
		if cfg.episodeFile != nil {
			files = []*sonarrlib.EpisodeFile{cfg.episodeFile}
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(files))
	})

	mux.HandleFunc("/MediaCover/", func(w http.ResponseWriter, r *http.Request) {
		ct := cfg.imageType
		if ct == "" {
			ct = "image/jpeg"
		}

		w.Header().Set("Content-Type", ct)
		_, _ = w.Write(cfg.imageBody)
	})

	return httptest.NewServer(mux)
}

func TestImportByFilePath(t *testing.T) {
	episodeParseOutput := &sonarrlib.ParseOutput{
		Title: "Breaking Bad",
		ParsedEpisodeInfo: &sonarrlib.ParsedEpisodeInfo{
			SeriesTitle:    "Breaking Bad",
			SeasonNumber:   1,
			EpisodeNumbers: []int{1},
		},
		Episodes: []*sonarrlib.Episode{
			{ID: 200, SeriesID: 10, SeasonNumber: 1, EpisodeNumber: 1},
		},
	}

	pendingRecord := &sonarrlib.QueueRecord{
		ID:                   1,
		EpisodeID:            200,
		DownloadID:           "nzb-abc123",
		TrackedDownloadState: "importPending",
	}

	tests := []struct {
		name                 string
		path                 string
		parseResp            *sonarrlib.ParseOutput
		queueResp            *sonarrlib.Queue
		queuePages           []*sonarrlib.Queue
		queueHTTPStatus      int
		wantCmdName          string
		wantCmdPath          string
		wantDownloadClientID string
		errFunc              require.ErrorAssertionFunc
	}{
		{
			name:                 "pending queue record found: downloadClientId included in command",
			path:                 "/tv/Breaking.Bad.S01E01.mkv",
			parseResp:            episodeParseOutput,
			queueResp:            &sonarrlib.Queue{Records: []*sonarrlib.QueueRecord{pendingRecord}},
			wantCmdName:          "DownloadedEpisodesScan",
			wantCmdPath:          "/tv/Breaking.Bad.S01E01.mkv",
			wantDownloadClientID: "nzb-abc123",
		},
		{
			name:        "episode unrecognized: command sent without downloadClientId",
			path:        "/tv/Breaking.Bad.S01E01.mkv",
			parseResp:   &sonarrlib.ParseOutput{},
			queueResp:   &sonarrlib.Queue{Records: []*sonarrlib.QueueRecord{pendingRecord}},
			wantCmdName: "DownloadedEpisodesScan",
			wantCmdPath: "/tv/Breaking.Bad.S01E01.mkv",
		},
		{
			name:        "no matching queue record: command sent without downloadClientId",
			path:        "/tv/Breaking.Bad.S01E01.mkv",
			parseResp:   episodeParseOutput,
			queueResp:   &sonarrlib.Queue{Records: []*sonarrlib.QueueRecord{}},
			wantCmdName: "DownloadedEpisodesScan",
			wantCmdPath: "/tv/Breaking.Bad.S01E01.mkv",
		},
		{
			// Sonarr already imported the episode on its own before we scanned.
			name:      "queue record already imported: command sent without downloadClientId",
			path:      "/tv/Breaking.Bad.S01E01.mkv",
			parseResp: episodeParseOutput,
			queueResp: &sonarrlib.Queue{Records: []*sonarrlib.QueueRecord{
				{ID: 1, EpisodeID: 200, DownloadID: "nzb-abc123", TrackedDownloadState: "imported"},
			}},
			wantCmdName: "DownloadedEpisodesScan",
			wantCmdPath: "/tv/Breaking.Bad.S01E01.mkv",
		},
		{
			// Queue API unreachable: fall back gracefully rather than blocking the import.
			name:            "queue API error: command sent without downloadClientId",
			path:            "/tv/Breaking.Bad.S01E01.mkv",
			parseResp:       episodeParseOutput,
			queueHTTPStatus: http.StatusInternalServerError,
			wantCmdName:     "DownloadedEpisodesScan",
			wantCmdPath:     "/tv/Breaking.Bad.S01E01.mkv",
		},
		{
			// Match found on second page: verifies pagination terminates early on hit.
			name:      "match on second queue page: downloadClientId from second page included",
			path:      "/tv/Breaking.Bad.S01E01.mkv",
			parseResp: episodeParseOutput,
			queuePages: []*sonarrlib.Queue{
				{
					TotalRecords: 2,
					Records: []*sonarrlib.QueueRecord{
						{ID: 99, EpisodeID: 999, DownloadID: "nzb-other", TrackedDownloadState: "importPending"},
					},
				},
				{
					TotalRecords: 2,
					Records:      []*sonarrlib.QueueRecord{pendingRecord},
				},
			},
			wantCmdName:          "DownloadedEpisodesScan",
			wantCmdPath:          "/tv/Breaking.Bad.S01E01.mkv",
			wantDownloadClientID: "nzb-abc123",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotCmd struct {
				Name             string `json:"name"`
				Path             string `json:"path"`
				DownloadClientId string `json:"downloadClientId"`
			}

			srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
				parseResp:       test.parseResp,
				queueResp:       test.queueResp,
				queuePages:      test.queuePages,
				queueHTTPStatus: test.queueHTTPStatus,
				onCommand: func(t *testing.T, r *http.Request) {
					require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCmd))
				},
			})
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			err := client.ImportByFilePath(t.Context(), test.path, 0)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, test.wantCmdName, gotCmd.Name)
				assert.Equal(t, test.wantCmdPath, gotCmd.Path)
				assert.Equal(t, test.wantDownloadClientID, gotCmd.DownloadClientId)
			}
		})
	}
}

func TestGetInfo_UsesFileStemAsTitleParam(t *testing.T) {
	var gotTitle, gotPath string

	knownParseOutput := &sonarrlib.ParseOutput{
		Title: "Colonel Bleep",
		ParsedEpisodeInfo: &sonarrlib.ParsedEpisodeInfo{
			SeriesTitle:    "Colonel Bleep",
			SeasonNumber:   1,
			EpisodeNumbers: []int{1},
		},
		Episodes: []*sonarrlib.Episode{
			{ID: 1, SeriesID: 10, SeasonNumber: 1, EpisodeNumber: 1},
		},
	}

	srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
		parseResp: knownParseOutput,
		onParse: func(t *testing.T, r *http.Request) {
			gotTitle = r.URL.Query().Get("title")
			gotPath = r.URL.Query().Get("path")
		},
	})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetInfo(t.Context(), "/downloads/Colonel.Bleep.S01E01.1080p.WEB-DL.mp4")
	require.NoError(t, err)

	assert.Equal(t, "Colonel.Bleep.S01E01.1080p.WEB-DL", gotTitle, "title param should be filename stem")
	assert.Empty(t, gotPath, "path param must not be sent")
}

func TestGetInfo_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, err := client.GetInfo(t.Context(), "/any/path.mkv")
	require.Error(t, err)
}

func TestImportByFilePath_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	err := client.ImportByFilePath(t.Context(), "/tv/Breaking.Bad.S01E01.mkv", 0)
	require.Error(t, err)
}

func TestImportByFilePath_BlocksUntilTerminalStatus(t *testing.T) {
	tests := []struct {
		name             string
		commandStatuses  []string
		commandResult    string
		commandHTTPError int
		commandMessage   string
		errFunc          require.ErrorAssertionFunc
		errSubstring     string
	}{
		{
			name:            "transitions queued then started then completed",
			commandStatuses: []string{"queued", "started", "completed"},
		},
		{
			name:            "completed with successful result is treated as success",
			commandStatuses: []string{"completed"},
			commandResult:   "successful",
		},
		{
			name:            "completed with unsuccessful result surfaces as error",
			commandStatuses: []string{"completed"},
			commandResult:   "unsuccessful",
			commandMessage:  "no eligible files",
			errFunc:         require.Error,
			errSubstring:    "no successful imports",
		},
		{
			name:            "terminal failed surfaces as error",
			commandStatuses: []string{"failed"},
			commandMessage:  "import rejected",
			errFunc:         require.Error,
			errSubstring:    "failed",
		},
		{
			name:            "terminal aborted surfaces as error",
			commandStatuses: []string{"started", "aborted"},
			errFunc:         require.Error,
			errSubstring:    "aborted",
		},
		{
			name:             "polling endpoint failure surfaces as error",
			commandHTTPError: http.StatusInternalServerError,
			errFunc:          require.Error,
			errSubstring:     "get sonarr command status",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errFunc == nil {
				test.errFunc = require.NoError
			}

			srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
				parseResp:               &sonarrlib.ParseOutput{},
				commandStatuses:         test.commandStatuses,
				commandResult:           test.commandResult,
				commandStatusMessage:    test.commandMessage,
				commandStatusHTTPStatus: test.commandHTTPError,
			})
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			err := client.ImportByFilePath(t.Context(), "/tv/Breaking.Bad.S01E01.mkv", 0)
			test.errFunc(t, err)

			if test.errSubstring != "" && err != nil {
				assert.Contains(t, err.Error(), test.errSubstring)
			}
		})
	}
}

// TestImportByFilePath_UnsuccessfulRecoversOnSizeMatch verifies the
// race-recovery post-check: when the scan command finishes with
// result="unsuccessful" but Sonarr's stored episode file size matches the
// caller's expectedSize, ImportByFilePath treats the scan as having
// succeeded (Sonarr's own completed-download handler already imported the
// file). When sizes disagree, the episode lacks a file, or the lookup
// fails, the original "no successful imports" error must propagate so the
// workflow retries.
func TestImportByFilePath_UnsuccessfulRecoversOnSizeMatch(t *testing.T) {
	const matchingSize int64 = 12345

	episodeParseOutput := &sonarrlib.ParseOutput{
		Title: "Breaking Bad",
		ParsedEpisodeInfo: &sonarrlib.ParsedEpisodeInfo{
			SeriesTitle:    "Breaking Bad",
			SeasonNumber:   1,
			EpisodeNumbers: []int{1},
		},
		Episodes: []*sonarrlib.Episode{
			{ID: 200, SeriesID: 10, SeasonNumber: 1, EpisodeNumber: 1},
		},
	}

	propagatesUnsuccessful := func(t require.TestingT, err error, msgAndArgs ...any) {
		require.Error(t, err, msgAndArgs...)
		assert.Contains(t, err.Error(), "no successful imports")
	}

	tests := []struct {
		name         string
		expectedSize int64
		parseResp    *sonarrlib.ParseOutput
		episodeByID  *sonarrlib.Episode
		episodeFile  *sonarrlib.EpisodeFile
		errFunc      require.ErrorAssertionFunc
	}{
		{
			name:         "size matches: scan treated as success",
			expectedSize: matchingSize,
			parseResp:    episodeParseOutput,
			episodeByID:  &sonarrlib.Episode{ID: 200, HasFile: true, EpisodeFileID: 7},
			episodeFile:  &sonarrlib.EpisodeFile{ID: 7, Size: matchingSize},
		},
		{
			name:         "size mismatches: original error propagates",
			expectedSize: matchingSize,
			parseResp:    episodeParseOutput,
			episodeByID:  &sonarrlib.Episode{ID: 200, HasFile: true, EpisodeFileID: 7},
			episodeFile:  &sonarrlib.EpisodeFile{ID: 7, Size: matchingSize + 1},
			errFunc:      propagatesUnsuccessful,
		},
		{
			name:         "episode has no file: original error propagates",
			expectedSize: matchingSize,
			parseResp:    episodeParseOutput,
			episodeByID:  &sonarrlib.Episode{ID: 200, HasFile: false},
			errFunc:      propagatesUnsuccessful,
		},
		{
			name:         "episode unidentifiable by parse: original error propagates",
			expectedSize: matchingSize,
			parseResp:    &sonarrlib.ParseOutput{},
			episodeByID:  nil,
			errFunc:      propagatesUnsuccessful,
		},
		{
			name:         "expectedSize zero disables post-check: original error propagates",
			expectedSize: 0,
			parseResp:    episodeParseOutput,
			episodeByID:  &sonarrlib.Episode{ID: 200, HasFile: true, EpisodeFileID: 7},
			episodeFile:  &sonarrlib.EpisodeFile{ID: 7, Size: matchingSize},
			errFunc:      propagatesUnsuccessful,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
				parseResp:            test.parseResp,
				episodeByID:          test.episodeByID,
				episodeFile:          test.episodeFile,
				commandStatuses:      []string{"completed"},
				commandResult:        "unsuccessful",
				commandStatusMessage: "no eligible files",
			})
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			err := client.ImportByFilePath(t.Context(), "/tv/Breaking.Bad.S01E01.mkv", test.expectedSize)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)
		})
	}
}

func TestGetInfo(t *testing.T) {
	knownParseOutput := &sonarrlib.ParseOutput{
		Title: "Breaking Bad",
		ParsedEpisodeInfo: &sonarrlib.ParsedEpisodeInfo{
			SeriesTitle:    "Breaking Bad",
			SeasonNumber:   1,
			EpisodeNumbers: []int{1},
			SeriesTitleInfo: &sonarrlib.SeriesTitleInfo{
				Year: 2008,
			},
		},
		Episodes: []*sonarrlib.Episode{
			{ID: 200, SeriesID: 10, SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"},
		},
	}

	tests := []struct {
		name       string
		parseResp  *sonarrlib.ParseOutput
		errFunc    require.ErrorAssertionFunc
		wantID     int64
		wantTitle  string
		wantYear   int
		wantSeries string
		wantSeason int
		wantEp     int
	}{
		{
			name:       "known path returns MediaInfo with correct fields",
			parseResp:  knownParseOutput,
			wantID:     200,
			wantTitle:  "Pilot",
			wantYear:   2008,
			wantSeries: "Breaking Bad",
			wantSeason: 1,
			wantEp:     1,
		},
		{
			name:      "unknown path returns ErrNotFound",
			parseResp: &sonarrlib.ParseOutput{},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.ErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newSonarrTestServer(t, test.parseResp)
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			info, err := client.GetInfo(t.Context(), "/tv/some.file.mkv")

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, test.wantID, info.GetID())
				assert.Equal(t, test.wantTitle, info.GetTitle())
				assert.Equal(t, test.wantYear, info.GetYear())
				assert.Equal(t, test.wantSeries, info.GetSeriesTitle())
				assert.Equal(t, test.wantSeason, info.GetSeasonNumber())
				assert.Equal(t, test.wantEp, info.GetEpisodeNumber())
			}
		})
	}
}

func TestGetPosterImage(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}

	knownParseOutput := &sonarrlib.ParseOutput{
		Title: "Breaking Bad",
		ParsedEpisodeInfo: &sonarrlib.ParsedEpisodeInfo{
			SeriesTitle:    "Breaking Bad",
			SeasonNumber:   1,
			EpisodeNumbers: []int{1},
		},
		Episodes: []*sonarrlib.Episode{
			{ID: 200, SeriesID: 10, SeasonNumber: 1, EpisodeNumber: 1},
		},
	}

	tests := []struct {
		name       string
		seriesByID *sonarrlib.Series
		imageBody  []byte
		imageType  string
		wantBytes  []byte
		wantMime   string
		errFunc    require.ErrorAssertionFunc
	}{
		{
			name: "JPEG series poster returned",
			seriesByID: &sonarrlib.Series{
				ID: 10,
				Images: []*starr.Image{
					{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/10/poster.jpg"},
				},
			},
			imageBody: jpegBytes,
			imageType: "image/jpeg",
			wantBytes: jpegBytes,
			wantMime:  "image/jpeg",
		},
		{
			name: "PNG series poster returned",
			seriesByID: &sonarrlib.Series{
				ID: 10,
				Images: []*starr.Image{
					{CoverType: "poster", Extension: ".png", URL: "/MediaCover/10/poster.png"},
				},
			},
			imageBody: pngBytes,
			imageType: "image/png",
			wantBytes: pngBytes,
			wantMime:  "image/png",
		},
		{
			name: "no poster image returns nil bytes",
			seriesByID: &sonarrlib.Series{
				ID: 10,
				Images: []*starr.Image{
					{CoverType: "fanart", Extension: ".jpg", URL: "/MediaCover/10/fanart.jpg"},
				},
			},
			imageBody: jpegBytes,
			imageType: "image/jpeg",
			wantBytes: nil,
			wantMime:  "",
		},
		{
			name: "unsupported image extension returns nil bytes",
			seriesByID: &sonarrlib.Series{
				ID: 10,
				Images: []*starr.Image{
					{CoverType: "poster", Extension: ".webp", URL: "/MediaCover/10/poster.webp"},
				},
			},
			imageBody: []byte("webp"),
			imageType: "image/webp",
			wantBytes: nil,
			wantMime:  "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
				parseResp:  knownParseOutput,
				seriesByID: test.seriesByID,
				imageBody:  test.imageBody,
				imageType:  test.imageType,
			})
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			gotBytes, gotMime, err := client.GetPosterImage(t.Context(), "/tv/Breaking.Bad.S01E01.mkv")

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)
			assert.Equal(t, test.wantBytes, gotBytes)
			assert.Equal(t, test.wantMime, gotMime)
		})
	}
}

func TestGetPosterImage_EpisodeNotFound(t *testing.T) {
	srv := newSonarrTestServer(t, &sonarrlib.ParseOutput{})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, _, err := client.GetPosterImage(t.Context(), "/tv/Unknown.S01E01.mkv")
	require.ErrorIs(t, err, medialib.ErrNotFound)
}

func TestGetPosterImage_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, _, err := client.GetPosterImage(t.Context(), "/tv/Breaking.Bad.S01E01.mkv")
	require.Error(t, err)
}

func TestGetInfo_ErrNotFoundSentinel(t *testing.T) {
	srv := newSonarrTestServer(t, &sonarrlib.ParseOutput{})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetInfo(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
