package artwork_test

import (
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

func TestFetchPosterImage_Unreachable(t *testing.T) {
	imgs := []*starr.Image{{CoverType: "poster", Extension: ".jpg", URL: "/MediaCover/1/poster.jpg"}}

	_, _, err := artwork.FetchPosterImage(t.Context(), imgs, "http://127.0.0.1:1", "key")
	require.Error(t, err)
}
