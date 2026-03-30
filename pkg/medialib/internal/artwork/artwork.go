// Package artwork provides shared utilities for fetching poster images from
// arr-style media library services (Radarr, Sonarr).
package artwork

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"golift.io/starr"
)

const (
	// MimeJPEG is the MIME type for JPEG images.
	MimeJPEG = "image/jpeg"
	// MimePNG is the MIME type for PNG images.
	MimePNG = "image/png"

	// fetchTimeout is the per-request deadline for poster image downloads.
	fetchTimeout = 10 * time.Second
)

// FetchPosterImage finds the poster image in images, fetches it from baseURL
// using the provided API key, and returns the bytes and MIME type.
// Returns nil bytes (no error) when no JPEG or PNG poster is available or
// the image type cannot be validated.
//
// The API key is only sent when the resolved image URL starts with baseURL.
// RemoteURL values (absolute external URLs) are fetched without the API key.
func FetchPosterImage(ctx context.Context, images []*starr.Image, baseURL, apiKey string) ([]byte, string, error) {
	normalizedBase := strings.TrimRight(baseURL, "/")

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
			imageURL = normalizedBase + imageURL
		}

		fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, imageURL, nil)
		if err != nil {
			return nil, "", fmt.Errorf("build image request: %w", err)
		}

		// Only send the API key when fetching from the configured arr instance.
		// RemoteURL values may point to external CDNs and must not receive it.
		if strings.HasPrefix(imageURL, normalizedBase) {
			req.Header.Set("X-Api-Key", apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("fetch poster image: %w", err)
		}

		defer func() { _ = resp.Body.Close() }()

		// Post-fetch Content-Type validation using the standard MIME parser.
		ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || (ct != MimeJPEG && ct != MimePNG) {
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
