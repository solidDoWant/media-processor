// Package artwork provides shared utilities for fetching poster images from
// arr-style media library services (Radarr, Sonarr).
package artwork

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golift.io/starr"
)

const (
	// MimeJPEG is the MIME type for JPEG images.
	MimeJPEG = "image/jpeg"
	// MimePNG is the MIME type for PNG images.
	MimePNG = "image/png"
)

// FetchPosterImage finds the poster image in images, fetches it from baseURL
// using the provided API key, and returns the bytes and MIME type.
// Returns nil bytes (no error) when no JPEG or PNG poster is available or
// the image type cannot be validated.
func FetchPosterImage(ctx context.Context, images []*starr.Image, baseURL, apiKey string) ([]byte, string, error) {
	for _, img := range images {
		if img.CoverType != "poster" {
			continue
		}

		// Pre-fetch extension check: only accept JPEG and PNG.
		ext := strings.ToLower(img.Extension)
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			return nil, "", nil
		}

		imageURL := img.URL
		if imageURL == "" {
			imageURL = img.RemoteURL
		}

		if imageURL == "" {
			return nil, "", nil
		}

		// Relative paths are served by the arr instance; prepend the base URL.
		if strings.HasPrefix(imageURL, "/") {
			imageURL = strings.TrimRight(baseURL, "/") + imageURL
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
		if err != nil {
			return nil, "", fmt.Errorf("build image request: %w", err)
		}

		req.Header.Set("X-Api-Key", apiKey)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("fetch poster image: %w", err)
		}

		defer func() { _ = resp.Body.Close() }()

		// Post-fetch Content-Type validation.
		ct := resp.Header.Get("Content-Type")
		// Strip parameters like "; charset=utf-8".
		if i := strings.Index(ct, ";"); i >= 0 {
			ct = strings.TrimSpace(ct[:i])
		}

		if ct != MimeJPEG && ct != MimePNG {
			return nil, "", nil
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("read poster image: %w", err)
		}

		return data, ct, nil
	}

	return nil, "", nil
}
