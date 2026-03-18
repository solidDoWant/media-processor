package radarr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	radarrlib "golift.io/starr/radarr"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/pkg/medialib/radarr"
)

func newTestServer(t *testing.T, movies []*radarrlib.Movie) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(movies))
	})

	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(&radarrlib.CommandResponse{ID: 1, Status: "queued"}))
	})

	return httptest.NewServer(mux)
}

// unusedURL returns a URL pointing at a port where nothing is listening.
func unusedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return fmt.Sprintf("http://%s", addr)
}

func TestGetMovieByFilePath(t *testing.T) {
	knownMovie := &radarrlib.Movie{
		ID:    42,
		Title: "The Matrix",
		Year:  1999,
		MovieFile: &radarrlib.MovieFile{
			ID:   1,
			Path: "/movies/The Matrix (1999)/The.Matrix.1999.mkv",
		},
	}

	tests := []struct {
		name     string
		path     string
		movies   []*radarrlib.Movie
		cfg      radarr.Config
		expected medialib.Movie
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:   "known path returns movie",
			path:   "/movies/The Matrix (1999)/The.Matrix.1999.mkv",
			movies: []*radarrlib.Movie{knownMovie},
			expected: medialib.Movie{
				ID:    42,
				Title: "The Matrix",
				Year:  1999,
			},
		},
		{
			name:   "unknown path returns ErrNotFound",
			path:   "/movies/Unknown/Unknown.mkv",
			movies: []*radarrlib.Movie{knownMovie},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.ErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
		{
			name:   "path translation maps local to remote path",
			path:   "/mnt/movies/The Matrix (1999)/The.Matrix.1999.mkv",
			movies: []*radarrlib.Movie{knownMovie},
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
			name:   "movie without file is skipped",
			path:   "/movies/Anything/anything.mkv",
			movies: []*radarrlib.Movie{{ID: 1, Title: "No File", Year: 2000}},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.ErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, tc.movies)
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

func TestRefreshMovie(t *testing.T) {
	srv := newTestServer(t, nil)
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	err := client.RefreshMovie(t.Context(), 42)
	require.NoError(t, err)
}

func TestGetMovieByFilePath_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unusedURL(t), APIKey: "test-key"})

	_, err := client.GetMovieByFilePath(t.Context(), "/any/path.mkv")
	require.Error(t, err)
}

func TestRefreshMovie_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unusedURL(t), APIKey: "test-key"})

	err := client.RefreshMovie(t.Context(), 42)
	require.Error(t, err)
}

func TestGetMovieByFilePath_ErrNotFoundSentinel(t *testing.T) {
	srv := newTestServer(t, []*radarrlib.Movie{})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetMovieByFilePath(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
