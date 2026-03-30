package radarr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golift.io/starr"
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

// testServerConfig holds configuration for test HTTP servers.
type testServerConfig struct {
	parseResp *parseResponse
	movieByID *radarrlib.Movie // response for /api/v3/movie/{id}
	imageBody []byte           // raw bytes served at image paths
	imageType string           // Content-Type for image responses
	// onCommand is called with the raw command request when /api/v3/command is hit.
	onCommand func(t *testing.T, r *http.Request)
}

func newTestServer(t *testing.T, parseResp *parseResponse) *httptest.Server {
	t.Helper()
	return newTestServerWithConfig(t, testServerConfig{parseResp: parseResp})
}

func newTestServerWithConfig(t *testing.T, cfg testServerConfig) *httptest.Server {
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
		require.NoError(t, json.NewEncoder(w).Encode(&radarrlib.CommandResponse{ID: 1, Status: "queued"}))
	})

	mux.HandleFunc("/api/v3/movie/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(cfg.movieByID))
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

func TestImportByFilePath(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		cfg          radarr.Config
		wantCmdName  string
		wantCmdPath  string
		errFunc      require.ErrorAssertionFunc
	}{
		{
			name:        "sends DownloadedMoviesScan with the given path",
			path:        "/movies/The.Matrix.1999.mkv",
			wantCmdName: "DownloadedMoviesScan",
			wantCmdPath: "/movies/The.Matrix.1999.mkv",
		},
		{
			name: "path translation maps local prefix to remote prefix in command",
			path: "/mnt/movies/The.Matrix.1999.mkv",
			cfg: radarr.Config{
				LocalPathPrefix:  "/mnt/movies",
				RemotePathPrefix: "/movies",
			},
			wantCmdName: "DownloadedMoviesScan",
			wantCmdPath: "/movies/The.Matrix.1999.mkv",
		},
		{
			name: "path traversal outside remote prefix returns error without sending command",
			path: "/mnt/movies/../../etc/passwd",
			cfg: radarr.Config{
				LocalPathPrefix:  "/mnt/movies",
				RemotePathPrefix: "/movies",
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

			srv := newTestServerWithConfig(t, testServerConfig{
				onCommand: func(t *testing.T, r *http.Request) {
					require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCmd))
				},
			})
			t.Cleanup(srv.Close)

			cfg := tc.cfg
			cfg.URL = srv.URL
			cfg.APIKey = "test-key"

			client := radarr.New(cfg)

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

func TestGetMovieByFilePath_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, err := client.GetMovieByFilePath(t.Context(), "/any/path.mkv")
	require.Error(t, err)
}

func TestImportByFilePath_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unreachableURL, APIKey: "test-key"})

	err := client.ImportByFilePath(t.Context(), "/movies/The.Matrix.1999.mkv")
	require.Error(t, err)
}

func TestGetInfo(t *testing.T) {
	tests := []struct {
		name      string
		parseResp *parseResponse
		errFunc   require.ErrorAssertionFunc
		wantID    int64
		wantTitle string
		wantYear  int
	}{
		{
			name:      "known path returns MediaInfo with correct fields",
			parseResp: &parseResponse{Movie: &radarrlib.Movie{ID: 42, Title: "The Matrix", Year: 1999}},
			wantID:    42,
			wantTitle: "The Matrix",
			wantYear:  1999,
		},
		{
			name:      "unknown path returns ErrNotFound",
			parseResp: &parseResponse{Movie: nil},
			errFunc: func(t require.TestingT, err error, msgAndArgs ...any) {
				require.ErrorIs(t, err, medialib.ErrNotFound, msgAndArgs...)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, tc.parseResp)
			t.Cleanup(srv.Close)

			client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

			info, err := client.GetInfo(t.Context(), "/movies/some.file.mkv")

			errFunc := tc.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, tc.wantID, info.GetID())
				assert.Equal(t, tc.wantTitle, info.GetTitle())
				assert.Equal(t, tc.wantYear, info.GetYear())
				assert.Equal(t, "", info.GetSeriesTitle())
				assert.Equal(t, 0, info.GetSeasonNumber())
				assert.Equal(t, 0, info.GetEpisodeNumber())
			}
		})
	}
}

func TestGetPosterImage(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}

	knownMovie := &radarrlib.Movie{ID: 42, Title: "The Matrix", Year: 1999}

	tests := []struct {
		name      string
		movieByID *radarrlib.Movie
		imageBody []byte
		imageType string
		wantBytes []byte
		wantMime  string
		errFunc   require.ErrorAssertionFunc
	}{
		{
			name: "JPEG poster image returned",
			movieByID: &radarrlib.Movie{
				ID: 42,
				Images: []*starr.Image{
					{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/42/poster.jpg"},
				},
			},
			imageBody: jpegBytes,
			imageType: "image/jpeg",
			wantBytes: jpegBytes,
			wantMime:  "image/jpeg",
		},
		{
			name: "PNG poster image returned",
			movieByID: &radarrlib.Movie{
				ID: 42,
				Images: []*starr.Image{
					{CoverType: "poster", Extension: ".png", URL: "/MediaCover/42/poster.png"},
				},
			},
			imageBody: pngBytes,
			imageType: "image/png",
			wantBytes: pngBytes,
			wantMime:  "image/png",
		},
		{
			name: "no poster image returns nil bytes",
			movieByID: &radarrlib.Movie{
				ID: 42,
				Images: []*starr.Image{
					{CoverType: "fanart", Extension: ".jpg", URL: "/MediaCover/42/fanart.jpg"},
				},
			},
			imageBody: jpegBytes,
			imageType: "image/jpeg",
			wantBytes: nil,
			wantMime:  "",
		},
		{
			name: "unsupported image extension returns nil bytes",
			movieByID: &radarrlib.Movie{
				ID: 42,
				Images: []*starr.Image{
					{CoverType: "poster", Extension: ".webp", URL: "/MediaCover/42/poster.webp"},
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
			srv := newTestServerWithConfig(t, testServerConfig{
				parseResp: &parseResponse{Movie: knownMovie},
				movieByID: tc.movieByID,
				imageBody: tc.imageBody,
				imageType: tc.imageType,
			})
			t.Cleanup(srv.Close)

			client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

			gotBytes, gotMime, err := client.GetPosterImage(t.Context(), "/movies/The.Matrix.1999.mkv")

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

func TestGetPosterImage_MovieNotFound(t *testing.T) {
	srv := newTestServer(t, &parseResponse{Movie: nil})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, _, err := client.GetPosterImage(t.Context(), "/movies/Unknown.mkv")
	require.ErrorIs(t, err, medialib.ErrNotFound)
}

func TestGetPosterImage_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, _, err := client.GetPosterImage(t.Context(), fmt.Sprintf("%s/movies/file.mkv", unreachableURL))
	require.Error(t, err)
}

func TestGetMovieByFilePath_ErrNotFoundSentinel(t *testing.T) {
	srv := newTestServer(t, &parseResponse{Movie: nil})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetMovieByFilePath(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
