package artwork_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golift.io/starr"

	"github.com/solidDoWant/media-processor/pkg/medialib/internal/artwork"
)

// imageServer returns a test HTTP server that serves the given body with the
// given Content-Type, requiring the X-Api-Key header to equal wantKey.
func imageServer(t *testing.T, wantKey, contentType string, body []byte) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
}

func TestFetchPosterImage(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}

	tests := []struct {
		name      string
		images    func(baseURL string) []*starr.Image
		wantBytes []byte
		wantMime  string
		errFunc   require.ErrorAssertionFunc
	}{
		{
			name: "JPEG poster image is returned",
			images: func(baseURL string) []*starr.Image {
				return []*starr.Image{{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster.jpg"}}
			},
			wantBytes: jpegBytes,
			wantMime:  "image/jpeg",
		},
		{
			name: "PNG poster image is returned",
			images: func(baseURL string) []*starr.Image {
				return []*starr.Image{{CoverType: "poster", Extension: ".png", URL: "/MediaCover/1/poster.png"}}
			},
			wantBytes: pngBytes,
			wantMime:  "image/png",
		},
		{
			name: "non-poster images are skipped",
			images: func(baseURL string) []*starr.Image {
				return []*starr.Image{
					{CoverType: "fanart", Extension: ".jpg", URL: "/MediaCover/1/fanart.jpg"},
				}
			},
			wantBytes: nil,
			wantMime:  "",
		},
		{
			name: "unsupported extension returns nil",
			images: func(baseURL string) []*starr.Image {
				return []*starr.Image{{CoverType: "poster", Extension: ".webp", URL: "/MediaCover/1/poster.webp"}}
			},
			wantBytes: nil,
			wantMime:  "",
		},
		{
			name: "empty extension returns nil",
			images: func(baseURL string) []*starr.Image {
				return []*starr.Image{{CoverType: "poster", Extension: "", URL: "/MediaCover/1/poster"}}
			},
			wantBytes: nil,
			wantMime:  "",
		},
		{
			name: "empty image list returns nil",
			images: func(baseURL string) []*starr.Image {
				return nil
			},
			wantBytes: nil,
			wantMime:  "",
		},
		{
			name: "absolute URL (RemoteURL) is used when URL is empty",
			images: func(baseURL string) []*starr.Image {
				return []*starr.Image{{CoverType: "poster", Extension: ".jpg", URL: "", RemoteURL: baseURL + "/remote/poster.jpg"}}
			},
			wantBytes: jpegBytes,
			wantMime:  "image/jpeg",
		},
		{
			name: "unsupported Content-Type from server returns nil",
			images: func(baseURL string) []*starr.Image {
				return []*starr.Image{{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster.jpg"}}
			},
			wantBytes: nil,
			wantMime:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var contentType string

			var respBody []byte

			switch tc.name {
			case "PNG poster image is returned":
				contentType = "image/png"
				respBody = pngBytes
			case "unsupported Content-Type from server returns nil":
				contentType = "image/webp"
				respBody = []byte("webp data")
			default:
				contentType = "image/jpeg"
				respBody = jpegBytes
			}

			srv := imageServer(t, "test-key", contentType, respBody)
			t.Cleanup(srv.Close)

			imgs := tc.images(srv.URL)

			gotBytes, gotMime, err := artwork.FetchPosterImage(t.Context(), imgs, srv.URL, "test-key")

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

// TestFetchPosterImage_RemoteURLExternalHost verifies that the API key is NOT
// forwarded when the image URL is a RemoteURL pointing to an external host.
// The external server is configured to reject requests that carry the key,
// so the test fails if the key is incorrectly forwarded.
func TestFetchPosterImage_RemoteURLExternalHost(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	// External CDN: rejects any request that carries an API key.
	externalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBytes)
	}))
	t.Cleanup(externalSrv.Close)

	imgs := []*starr.Image{{
		CoverType: "poster",
		Extension: ".jpg",
		URL:       "",
		RemoteURL: externalSrv.URL + "/poster.jpg",
	}}

	// baseURL is different from the external server — so the key must not be sent.
	gotBytes, gotMime, err := artwork.FetchPosterImage(t.Context(), imgs, "http://radarr.local:7878", "secret-key")

	require.NoError(t, err)
	assert.Equal(t, jpegBytes, gotBytes)
	assert.Equal(t, "image/jpeg", gotMime)
}

// TestFetchPosterImage_CrossHostRedirectDoesNotSendAPIKey verifies that the API
// key is not forwarded when the arr server issues a redirect to an external host.
// The external server records whether it received the key; the test asserts it did not.
func TestFetchPosterImage_CrossHostRedirectDoesNotSendAPIKey(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	var keyReceived bool

	// External server: records whether it received the API key.
	externalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			keyReceived = true
		}

		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBytes)
	}))
	t.Cleanup(externalSrv.Close)

	// Arr server: responds to poster requests with a redirect to the external server.
	arrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, externalSrv.URL+"/poster.jpg", http.StatusFound)
	}))
	t.Cleanup(arrSrv.Close)

	imgs := []*starr.Image{{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster.jpg"}}

	gotBytes, gotMime, err := artwork.FetchPosterImage(t.Context(), imgs, arrSrv.URL, "secret-key")

	require.NoError(t, err)
	assert.Equal(t, jpegBytes, gotBytes, "redirect must be followed and image returned")
	assert.Equal(t, "image/jpeg", gotMime)
	assert.False(t, keyReceived, "API key must not be forwarded to external redirect target")
}

func TestFetchPosterImage_Unreachable(t *testing.T) {
	imgs := []*starr.Image{{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster.jpg"}}

	_, _, err := artwork.FetchPosterImage(t.Context(), imgs, "http://127.0.0.1:1", "key")
	require.Error(t, err)
}

// TestFetchPosterImage_FallsBackToNextCandidate verifies that a rejected
// candidate (unsupported extension, bad Content-Type, non-200 status) causes
// the function to continue to the next poster image rather than returning
// immediately with nil.
func TestFetchPosterImage_FallsBackToNextCandidate(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	srv := imageServer(t, "test-key", "image/jpeg", jpegBytes)
	t.Cleanup(srv.Close)

	// The first image has an unsupported extension; the second is a valid JPEG.
	imgs := []*starr.Image{
		{CoverType: "poster", Extension: ".webp", URL: "/MediaCover/1/poster.webp"},
		{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster.jpg"},
	}

	gotBytes, gotMime, err := artwork.FetchPosterImage(t.Context(), imgs, srv.URL, "test-key")

	require.NoError(t, err)
	assert.Equal(t, jpegBytes, gotBytes, "second candidate must be returned when first is rejected")
	assert.Equal(t, "image/jpeg", gotMime)
}

// TestFetchPosterImage_OversizedResponseSkipped verifies that a response body
// larger than MaxPosterBytes is skipped rather than silently truncated.
func TestFetchPosterImage_OversizedResponseSkipped(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	// oversizedBody is one byte more than the allowed cap.
	oversizedBody := bytes.Repeat([]byte{0xFF}, artwork.MaxPosterBytes+1)

	var callCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		switch callCount {
		case 1:
			// First request: return an oversized body.
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(oversizedBody)
		default:
			// Subsequent requests: return a valid small JPEG.
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegBytes)
		}
	}))
	t.Cleanup(srv.Close)

	imgs := []*starr.Image{
		{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster-large.jpg"},
		{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster-small.jpg"},
	}

	gotBytes, gotMime, err := artwork.FetchPosterImage(t.Context(), imgs, srv.URL, "")

	require.NoError(t, err)
	assert.Equal(t, jpegBytes, gotBytes, "oversized first candidate must be skipped; second must be returned")
	assert.Equal(t, "image/jpeg", gotMime)
}
