package sonarr_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type sonarrFixture struct {
	series       []*sonarrlib.Series
	episodeFiles map[int64][]*sonarrlib.EpisodeFile // keyed by seriesID
	episodes     map[int64][]*sonarrlib.Episode     // keyed by episodeFileID
}

func newSonarrTestServer(t *testing.T, fix sonarrFixture) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(fix.series))
	})

	mux.HandleFunc("/api/v3/episodeFile", func(w http.ResponseWriter, r *http.Request) {
		seriesIDStr := r.URL.Query().Get("seriesId")
		seriesID, err := strconv.ParseInt(seriesIDStr, 10, 64)
		require.NoError(t, err)

		files := fix.episodeFiles[seriesID]
		if files == nil {
			files = []*sonarrlib.EpisodeFile{}
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(files))
	})

	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		fileIDStr := r.URL.Query().Get("episodeFileId")
		fileID, err := strconv.ParseInt(fileIDStr, 10, 64)
		require.NoError(t, err)

		eps := fix.episodes[fileID]
		if eps == nil {
			eps = []*sonarrlib.Episode{}
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(eps))
	})

	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(&sonarrlib.CommandResponse{ID: 1, Status: "queued"}))
	})

	return httptest.NewServer(mux)
}

func TestGetEpisodeByFilePath(t *testing.T) {
	fix := sonarrFixture{
		series: []*sonarrlib.Series{
			{ID: 10, Title: "Breaking Bad", Path: "/tv/Breaking Bad"},
		},
		episodeFiles: map[int64][]*sonarrlib.EpisodeFile{
			10: {
				{ID: 100, SeriesID: 10, Path: "/tv/Breaking Bad/Season 01/S01E01.mkv"},
			},
		},
		episodes: map[int64][]*sonarrlib.Episode{
			100: {
				{ID: 200, SeriesID: 10, SeasonNumber: 1, EpisodeNumber: 1},
			},
		},
	}

	tests := []struct {
		name     string
		path     string
		cfg      sonarr.Config
		expected medialib.Episode
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name: "known path returns episode",
			path: "/tv/Breaking Bad/Season 01/S01E01.mkv",
			expected: medialib.Episode{
				ID:            200,
				SeriesTitle:   "Breaking Bad",
				SeasonNumber:  1,
				EpisodeNumber: 1,
			},
		},
		{
			name: "unknown path returns ErrNotFound",
			path: "/tv/Unknown/S01E01.mkv",
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.ErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
		{
			name: "path translation maps local to remote path",
			path: "/mnt/tv/Breaking Bad/Season 01/S01E01.mkv",
			cfg: sonarr.Config{
				LocalPathPrefix:  "/mnt/tv",
				RemotePathPrefix: "/tv",
			},
			expected: medialib.Episode{
				ID:            200,
				SeriesTitle:   "Breaking Bad",
				SeasonNumber:  1,
				EpisodeNumber: 1,
			},
		},
		{
			name: "path traversal outside remote prefix returns error",
			path: "/mnt/tv/../../etc/passwd",
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
			srv := newSonarrTestServer(t, fix)
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

func TestRefreshSeries(t *testing.T) {
	srv := newSonarrTestServer(t, sonarrFixture{})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	err := client.RefreshSeries(t.Context(), 10)
	require.NoError(t, err)
}

func TestGetEpisodeByFilePath_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, err := client.GetEpisodeByFilePath(t.Context(), "/any/path.mkv")
	require.Error(t, err)
}

func TestRefreshSeries_UnreachableURL(t *testing.T) {
	client := sonarr.New(sonarr.Config{URL: unreachableURL, APIKey: "test-key"})

	err := client.RefreshSeries(t.Context(), 10)
	require.Error(t, err)
}

func TestGetEpisodeByFilePath_ErrNotFoundSentinel(t *testing.T) {
	srv := newSonarrTestServer(t, sonarrFixture{
		series: []*sonarrlib.Series{},
	})
	t.Cleanup(srv.Close)

	client := sonarr.New(sonarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetEpisodeByFilePath(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
