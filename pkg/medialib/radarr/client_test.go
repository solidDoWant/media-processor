package radarr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// fastPollInterval keeps tests that exercise the command polling loop snappy.
const fastPollInterval = time.Millisecond

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
	// commandStatuses is the sequence of status values returned by GET
	// /api/v3/command/{id} on successive polls. The last entry is reused for
	// further polls. Empty defaults to a single "completed" entry.
	commandStatuses []string
	// commandResult is the value of the response's "result" field. Radarr
	// returns "unsuccessful" when a command finishes but reports no useful
	// work (e.g. import that found nothing eligible). Empty leaves the field
	// out of the response.
	commandResult string
	// commandStatusMessage is the message attached to status responses.
	commandStatusMessage string
	// commandStatusHTTPStatus, when non-zero, is returned by GET
	// /api/v3/command/{id} instead of the JSON body.
	commandStatusHTTPStatus int
	// onCommand is called with the raw command request when /api/v3/command is hit.
	onCommand func(t *testing.T, r *http.Request)
	// onParse is called with the raw parse request when /api/v3/parse is hit.
	onParse func(t *testing.T, r *http.Request)
}

func newTestServer(t *testing.T, parseResp *parseResponse) *httptest.Server {
	t.Helper()
	return newTestServerWithConfig(t, testServerConfig{parseResp: parseResp})
}

func newTestServerWithConfig(t *testing.T, cfg testServerConfig) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/parse", func(w http.ResponseWriter, r *http.Request) {
		if cfg.onParse != nil {
			cfg.onParse(t, r)
		}

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

	// /api/v3/command/{id} drives the post-submit polling loop. Default
	// behavior is to immediately return a terminal "completed" so existing
	// tests don't have to opt in.
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

func TestImportByFilePath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantCmdName string
		wantCmdPath string
		errFunc     require.ErrorAssertionFunc
	}{
		{
			name:        "sends DownloadedMoviesScan with the given path",
			path:        "/movies/The.Matrix.1999.mkv",
			wantCmdName: "DownloadedMoviesScan",
			wantCmdPath: "/movies/The.Matrix.1999.mkv",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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

			client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			err := client.ImportByFilePath(t.Context(), test.path, 0)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, test.wantCmdName, gotCmd.Name)
				assert.Equal(t, test.wantCmdPath, gotCmd.Path)
			}
		})
	}
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
			errSubstring:     "get radarr command status",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errFunc == nil {
				test.errFunc = require.NoError
			}

			srv := newTestServerWithConfig(t, testServerConfig{
				commandStatuses:         test.commandStatuses,
				commandResult:           test.commandResult,
				commandStatusMessage:    test.commandMessage,
				commandStatusHTTPStatus: test.commandHTTPError,
			})
			t.Cleanup(srv.Close)

			client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			err := client.ImportByFilePath(t.Context(), "/movies/The.Matrix.1999.mkv", 0)
			test.errFunc(t, err)

			if test.errSubstring != "" && err != nil {
				assert.Contains(t, err.Error(), test.errSubstring)
			}
		})
	}
}

// TestImportByFilePath_UnsuccessfulRecoversOnSizeMatch verifies the
// race-recovery post-check: when the scan command finishes with
// result="unsuccessful" but Radarr's stored movie file size matches the
// caller's expectedSize, ImportByFilePath treats the scan as having
// succeeded (Radarr's own completed-download handler already imported the
// file). When sizes disagree, the movie lacks a file, or the lookup fails,
// the original "no successful imports" error must propagate so the
// workflow retries.
func TestImportByFilePath_UnsuccessfulRecoversOnSizeMatch(t *testing.T) {
	const matchingSize int64 = 67890

	knownMovie := &radarrlib.Movie{ID: 42, Title: "The Matrix", Year: 1999}

	propagatesUnsuccessful := func(t require.TestingT, err error, msgAndArgs ...any) {
		require.Error(t, err, msgAndArgs...)
		assert.Contains(t, err.Error(), "no successful imports")
	}

	propagatesFileMismatch := func(t require.TestingT, err error, msgAndArgs ...any) {
		require.Error(t, err, msgAndArgs...)
		assert.True(t, errors.Is(err, medialib.ErrLibraryFileMismatch), "expected ErrLibraryFileMismatch, got: %v", err)
	}

	tests := []struct {
		name         string
		expectedSize int64
		parseResp    *parseResponse
		movieByID    *radarrlib.Movie
		errFunc      require.ErrorAssertionFunc
	}{
		{
			name:         "size matches: scan treated as success",
			expectedSize: matchingSize,
			parseResp:    &parseResponse{Movie: knownMovie},
			movieByID:    &radarrlib.Movie{ID: 42, HasFile: true, MovieFile: &radarrlib.MovieFile{Size: matchingSize}},
		},
		{
			name:         "size mismatches: ErrLibraryFileMismatch returned",
			expectedSize: matchingSize,
			parseResp:    &parseResponse{Movie: knownMovie},
			movieByID:    &radarrlib.Movie{ID: 42, HasFile: true, MovieFile: &radarrlib.MovieFile{Size: matchingSize + 1}},
			errFunc:      propagatesFileMismatch,
		},
		{
			name:         "movie has no file: original error propagates",
			expectedSize: matchingSize,
			parseResp:    &parseResponse{Movie: knownMovie},
			movieByID:    &radarrlib.Movie{ID: 42, HasFile: false},
			errFunc:      propagatesUnsuccessful,
		},
		{
			name:         "movie unidentifiable by parse: original error propagates",
			expectedSize: matchingSize,
			parseResp:    &parseResponse{Movie: nil},
			movieByID:    nil,
			errFunc:      propagatesUnsuccessful,
		},
		{
			name:         "expectedSize zero disables post-check: original error propagates",
			expectedSize: 0,
			parseResp:    &parseResponse{Movie: knownMovie},
			movieByID:    &radarrlib.Movie{ID: 42, HasFile: true, MovieFile: &radarrlib.MovieFile{Size: matchingSize}},
			errFunc:      propagatesUnsuccessful,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServerWithConfig(t, testServerConfig{
				parseResp:            test.parseResp,
				movieByID:            test.movieByID,
				commandStatuses:      []string{"completed"},
				commandResult:        "unsuccessful",
				commandStatusMessage: "no eligible files",
			})
			t.Cleanup(srv.Close)

			client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key", CommandPollInterval: fastPollInterval})

			err := client.ImportByFilePath(t.Context(), "/movies/The.Matrix.1999.mkv", test.expectedSize)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)
		})
	}
}

func TestGetInfo_UsesFileStemAsTitleParam(t *testing.T) {
	var gotTitle, gotPath string

	knownMovie := &radarrlib.Movie{ID: 1, Title: "Big Buck Bunny", Year: 2008}

	srv := newTestServerWithConfig(t, testServerConfig{
		parseResp: &parseResponse{Movie: knownMovie},
		onParse: func(t *testing.T, r *http.Request) {
			gotTitle = r.URL.Query().Get("title")
			gotPath = r.URL.Query().Get("path")
		},
	})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetInfo(t.Context(), "/downloads/Big.Buck.Bunny.2008.1080p.WEB-DL.mp4")
	require.NoError(t, err)

	assert.Equal(t, "Big.Buck.Bunny.2008.1080p.WEB-DL", gotTitle, "title param should be filename stem")
	assert.Empty(t, gotPath, "path param must not be sent")
}

func TestGetInfo_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unreachableURL, APIKey: "test-key"})

	_, err := client.GetInfo(t.Context(), "/any/path.mkv")
	require.Error(t, err)
}

func TestImportByFilePath_UnreachableURL(t *testing.T) {
	client := radarr.New(radarr.Config{URL: unreachableURL, APIKey: "test-key"})

	err := client.ImportByFilePath(t.Context(), "/movies/The.Matrix.1999.mkv", 0)
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

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t, test.parseResp)
			t.Cleanup(srv.Close)

			client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

			info, err := client.GetInfo(t.Context(), "/movies/some.file.mkv")

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			errFunc(t, err)

			if err == nil {
				assert.Equal(t, test.wantID, info.GetID())
				assert.Equal(t, test.wantTitle, info.GetTitle())
				assert.Equal(t, test.wantYear, info.GetYear())
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

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServerWithConfig(t, testServerConfig{
				parseResp: &parseResponse{Movie: knownMovie},
				movieByID: test.movieByID,
				imageBody: test.imageBody,
				imageType: test.imageType,
			})
			t.Cleanup(srv.Close)

			client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

			gotBytes, gotMime, err := client.GetPosterImage(t.Context(), "/movies/The.Matrix.1999.mkv")

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

func TestGetInfo_ErrNotFoundSentinel(t *testing.T) {
	srv := newTestServer(t, &parseResponse{Movie: nil})
	t.Cleanup(srv.Close)

	client := radarr.New(radarr.Config{URL: srv.URL, APIKey: "test-key"})

	_, err := client.GetInfo(t.Context(), "/no/such/file.mkv")
	require.True(t, errors.Is(err, medialib.ErrNotFound), "expected ErrNotFound, got %v", err)
}
