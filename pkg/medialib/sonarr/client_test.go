package sonarr_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

// sonarrTestServerConfig holds configuration for test HTTP servers.
type sonarrTestServerConfig struct {
	parseResp       *sonarrlib.ParseOutput
	seriesByID      *sonarrlib.Series // response for /api/v3/series/{id}
	queueResp       *sonarrlib.Queue  // response for /api/v3/queue; nil returns empty queue
	queueHTTPStatus int               // override HTTP status for /api/v3/queue; 0 means 200
	imageBody       []byte
	imageType       string
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

		if status == http.StatusOK {
			q := cfg.queueResp
			if q == nil {
				q = &sonarrlib.Queue{Records: []*sonarrlib.QueueRecord{}}
			}

			require.NoError(t, json.NewEncoder(w).Encode(q))
		}
	})

	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		if cfg.onCommand != nil {
			cfg.onCommand(t, r)
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(&sonarrlib.CommandResponse{ID: 1, Status: "queued"}))
	})

	mux.HandleFunc("/api/v3/series/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(cfg.seriesByID))
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotCmd struct {
				Name             string `json:"name"`
				Path             string `json:"path"`
				DownloadClientId string `json:"downloadClientId"`
			}

			srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
				parseResp:       tc.parseResp,
				queueResp:       tc.queueResp,
				queueHTTPStatus: tc.queueHTTPStatus,
				onCommand: func(t *testing.T, r *http.Request) {
					require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCmd))
				},
			})
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

			err := client.ImportByFilePath(t.Context(), tc.path)

			errFunc := tc.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, tc.wantCmdName, gotCmd.Name)
				assert.Equal(t, tc.wantCmdPath, gotCmd.Path)
				assert.Equal(t, tc.wantDownloadClientID, gotCmd.DownloadClientId)
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

	err := client.ImportByFilePath(t.Context(), "/tv/Breaking.Bad.S01E01.mkv")
	require.Error(t, err)
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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSonarrTestServer(t, tc.parseResp)
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

			info, err := client.GetInfo(t.Context(), "/tv/some.file.mkv")

			errFunc := tc.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, tc.wantID, info.GetID())
				assert.Equal(t, tc.wantTitle, info.GetTitle())
				assert.Equal(t, tc.wantYear, info.GetYear())
				assert.Equal(t, tc.wantSeries, info.GetSeriesTitle())
				assert.Equal(t, tc.wantSeason, info.GetSeasonNumber())
				assert.Equal(t, tc.wantEp, info.GetEpisodeNumber())
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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
				parseResp:  knownParseOutput,
				seriesByID: tc.seriesByID,
				imageBody:  tc.imageBody,
				imageType:  tc.imageType,
			})
			t.Cleanup(srv.Close)

			client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

			gotBytes, gotMime, err := client.GetPosterImage(t.Context(), "/tv/Breaking.Bad.S01E01.mkv")

			errFunc := tc.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)
			assert.Equal(t, tc.wantBytes, gotBytes)
			assert.Equal(t, tc.wantMime, gotMime)
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
