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
	parseResp  *sonarrlib.ParseOutput
	seriesByID *sonarrlib.Series // response for /api/v3/series/{id}
	imageBody  []byte
	imageType  string
	// onCommand is called with the raw command request when /api/v3/command is hit.
	onCommand func(t *testing.T, r *http.Request)
}

func newSonarrTestServer(t *testing.T, parseResp *sonarrlib.ParseOutput) *httptest.Server {
	t.Helper()
	return newSonarrTestServerWithConfig(t, sonarrTestServerConfig{parseResp: parseResp})
}

func newSonarrTestServerWithConfig(t *testing.T, cfg sonarrTestServerConfig) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/parse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(cfg.parseResp))
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

func TestGetEpisodeByFilePath(t *testing.T) {
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
		name      string
		path      string
		cfg       sonarr.Config
		parseResp *sonarrlib.ParseOutput
		expected  medialib.Episode
		errFunc   require.ErrorAssertionFunc
	}{
		{
			name:      "known path returns episode",
			path:      "/tv/Breaking.Bad.S01E01.mkv",
			parseResp: knownParseOutput,
			expected: medialib.Episode{
				ID:            200,
				SeriesID:      10,
				SeriesTitle:   "Breaking Bad",
				SeasonNumber:  1,
				EpisodeNumber: 1,
			},
		},
		{
			name:      "unrecognized path returns ErrNotFound",
			path:      "/tv/Unknown.S01E01.mkv",
			parseResp: &sonarrlib.ParseOutput{},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.ErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
		{
			name:      "path translation maps local to remote path",
			path:      "/mnt/tv/Breaking.Bad.S01E01.mkv",
			parseResp: knownParseOutput,
			cfg: sonarr.Config{
				LocalPathPrefix:  "/mnt/tv",
				RemotePathPrefix: "/tv",
			},
			expected: medialib.Episode{
				ID:            200,
				SeriesID:      10,
				SeriesTitle:   "Breaking Bad",
				SeasonNumber:  1,
				EpisodeNumber: 1,
			},
		},
		{
			name:      "path traversal outside remote prefix returns error",
			path:      "/mnt/tv/../../etc/passwd",
			parseResp: &sonarrlib.ParseOutput{},
			cfg: sonarr.Config{
				LocalPathPrefix:  "/mnt/tv",
				RemotePathPrefix: "/tv",
			},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.Error(t, err, msgAndArgs...)
				require.NotErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSonarrTestServer(t, tc.parseResp)
			t.Cleanup(srv.Close)

			cfg := tc.cfg
			cfg.URL = srv.URL
			cfg.APIKey = "test-key"

			client := sonarr.New(cfg)

			episode, err := client.GetEpisodeByFilePath(t.Context(), tc.path)

			errFunc := tc.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, tc.expected, episode)
			}
		})
	}
}

func TestImportByFilePath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		cfg         sonarr.Config
		wantCmdName string
		wantCmdPath string
		errFunc     require.ErrorAssertionFunc
	}{
		{
			name:        "sends DownloadedEpisodesScan with the given path",
			path:        "/tv/Breaking.Bad.S01E01.mkv",
			wantCmdName: "DownloadedEpisodesScan",
			wantCmdPath: "/tv/Breaking.Bad.S01E01.mkv",
		},
		{
			name: "path translation maps local prefix to remote prefix in command",
			path: "/mnt/tv/Breaking.Bad.S01E01.mkv",
			cfg: sonarr.Config{
				LocalPathPrefix:  "/mnt/tv",
				RemotePathPrefix: "/tv",
			},
			wantCmdName: "DownloadedEpisodesScan",
			wantCmdPath: "/tv/Breaking.Bad.S01E01.mkv",
		},
		{
			name: "path traversal outside remote prefix returns error without sending command",
			path: "/mnt/tv/../../etc/passwd",
			cfg: sonarr.Config{
				LocalPathPrefix:  "/mnt/tv",
				RemotePathPrefix: "/tv",
			},
			errFunc: require.Error,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotCmd struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}

			srv := newSonarrTestServerWithConfig(t, sonarrTestServerConfig{
				onCommand: func(t *testing.T, r *http.Request) {
					require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCmd))
				},
			})
			t.Cleanup(srv.Close)

			cfg := tc.cfg
			cfg.URL = srv.URL
			cfg.APIKey = "test-key"

			client := sonarr.New(cfg)

			err := client.ImportByFilePath(t.Context(), tc.path)

			errFunc := tc.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, tc.wantCmdName, gotCmd.Name)
				assert.Equal(t, tc.wantCmdPath, gotCmd.Path)
			}
		})
	}
}

func TestGetEpisodeByFilePath_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, err := client.GetEpisodeByFilePath(t.Context(), "/any/path.mkv")
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

func TestGetEpisodeByFilePath_ErrNotFoundSentinel(t *testing.T) {
	srv := newSonarrTestServer(t, &sonarrlib.ParseOutput{})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetEpisodeByFilePath(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
