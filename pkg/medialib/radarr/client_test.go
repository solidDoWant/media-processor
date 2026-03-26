package radarr_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	radarrlib "golift.io/starr/radarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/radarr"
)

// unreachableURL is a URL that always refuses connections (port 1 is a
// privileged port with no listener in any normal environment).
const unreachableURL = "http://127.0.0.1:1"

// parseResponse mirrors the shape of Radarr's /api/v3/parse response.
type parseResponse struct {
	Movie *radarrlib.Movie `json:"movie"`
}

func newTestServer(t *testing.T, parseResp *parseResponse) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/parse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(parseResp))
	})

	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(&radarrlib.CommandResponse{ID: 1, Status: "queued"}))
	})

	return httptest.NewServer(mux)
}

func TestGetMovieByFilePath(t *testing.T) {
	knownMovie := &radarrlib.Movie{
		ID:    42,
		Title: "The Matrix",
		Year:  1999,
	}

	tests := []struct {
		name      string
		path      string
		cfg       radarr.Config
		parseResp *parseResponse
		expected  medialib.Movie
		errFunc   require.ErrorAssertionFunc
	}{
		{
			name:      "known path returns movie",
			path:      "/movies/The.Matrix.1999.mkv",
			parseResp: &parseResponse{Movie: knownMovie},
			expected: medialib.Movie{
				ID:    42,
				Title: "The Matrix",
				Year:  1999,
			},
		},
		{
			name:      "unknown path returns ErrNotFound",
			path:      "/movies/Unknown.mkv",
			parseResp: &parseResponse{Movie: nil},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.ErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
		{
			name:      "path translation maps local to remote path",
			path:      "/mnt/movies/The.Matrix.1999.mkv",
			parseResp: &parseResponse{Movie: knownMovie},
			cfg: radarr.Config{
				LocalPathPrefix:  "/mnt/movies",
				RemotePathPrefix: "/movies",
			},
			expected: medialib.Movie{
				ID:    42,
				Title: "The Matrix",
				Year:  1999,
			},
		},
		{
			name:      "path traversal outside remote prefix returns error",
			path:      "/mnt/movies/../../etc/passwd",
			parseResp: &parseResponse{},
			cfg: radarr.Config{
				LocalPathPrefix:  "/mnt/movies",
				RemotePathPrefix: "/movies",
			},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.Error(t, err, msgAndArgs...)
				require.NotErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, tc.parseResp)
			t.Cleanup(srv.Close)

			cfg := tc.cfg
			cfg.URL = srv.URL
			cfg.APIKey = "test-key"

			client := radarr.New(cfg)

			movie, err := client.GetMovieByFilePath(t.Context(), tc.path)

			errFunc := tc.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}
			errFunc(t, err)

			if err == nil {
				assert.Equal(t, tc.expected, movie)
			}
		})
	}
}

func TestRefreshByFilePath(t *testing.T) {
	srv := newTestServer(t, &parseResponse{Movie: &radarrlib.Movie{ID: 42, Title: "The Matrix", Year: 1999}})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	err := client.RefreshByFilePath(t.Context(), "/movies/The.Matrix.1999.mkv")
	require.NoError(t, err)
}

func TestRefreshByFilePath_ErrNotFound(t *testing.T) {
	srv := newTestServer(t, &parseResponse{Movie: nil})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	err := client.RefreshByFilePath(t.Context(), "/movies/Unknown.mkv")
	require.ErrorIs(t, err, medialib.ErrNotFound)
}

func TestGetMovieByFilePath_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, err := client.GetMovieByFilePath(t.Context(), "/any/path.mkv")
	require.Error(t, err)
}

func TestRefreshByFilePath_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unreachableURL, APIKey: "test-key"})

	err := client.RefreshByFilePath(t.Context(), "/movies/The.Matrix.1999.mkv")
	require.Error(t, err)
}

func TestGetMovieByFilePath_ErrNotFoundSentinel(t *testing.T) {
	srv := newTestServer(t, &parseResponse{Movie: nil})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetMovieByFilePath(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
