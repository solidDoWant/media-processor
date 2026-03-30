package sonarr_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sonarrlib "golift.io/starr/sonarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/sonarr"
)

// unreachableURL is a URL that always refuses connections (port 1 is a
// privileged port with no listener in any normal environment).
const unreachableURL = "http://127.0.0.1:1"

func newSonarrTestServer(t *testing.T, parseResp *sonarrlib.ParseOutput) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/parse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(parseResp))
	})

	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(&sonarrlib.CommandResponse{ID: 1, Status: "queued"}))
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

func TestRefreshByFilePath(t *testing.T) {
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

	srv := newSonarrTestServer(t, knownParseOutput)
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	err := client.RefreshByFilePath(t.Context(), "/tv/Breaking.Bad.S01E01.mkv")
	require.NoError(t, err)
}

func TestRefreshByFilePath_ErrNotFound(t *testing.T) {
	srv := newSonarrTestServer(t, &sonarrlib.ParseOutput{})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	err := client.RefreshByFilePath(t.Context(), "/tv/Unknown.S01E01.mkv")
	require.ErrorIs(t, err, medialib.ErrNotFound)
}

func TestGetEpisodeByFilePath_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, err := client.GetEpisodeByFilePath(t.Context(), "/any/path.mkv")
	require.Error(t, err)
}

func TestRefreshByFilePath_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	err := client.RefreshByFilePath(t.Context(), "/tv/Breaking.Bad.S01E01.mkv")
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

func TestGetEpisodeByFilePath_ErrNotFoundSentinel(t *testing.T) {
	srv := newSonarrTestServer(t, &sonarrlib.ParseOutput{})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetEpisodeByFilePath(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
