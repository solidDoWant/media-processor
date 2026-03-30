// Package artwork provides shared utilities for fetching poster images from
// arr-style media library services (Radarr, Sonarr).
package artwork

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
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

	// MaxPosterBytes caps the response body read to prevent memory exhaustion
	// from unexpectedly large responses (e.g. from an external RemoteURL).
	// Responses larger than this limit are skipped as if no poster were available.
	MaxPosterBytes = 10 << 20 // 10 MiB
)

// FetchPosterImage finds the poster image in images, fetches it from baseURL
// using the provided API key, and returns the bytes and MIME type.
// Returns nil bytes (no error) when no JPEG or PNG poster is available or
// the image type cannot be validated.
//
// The API key is only sent for requests (including redirects) whose URL shares
// the same scheme, host, and path prefix as baseURL. Redirects that leave the
// baseURL origin are still followed, but the API key header is stripped before
// the redirected request is sent to ensure credentials are never forwarded to
// an external host.
func FetchPosterImage(ctx context.Context, images []*starr.Image, baseURL, apiKey string) ([]byte, string, error) {
	baseU, err := url.Parse(baseURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse base URL: %w", err)
	}

	// Normalize the base path for prefix comparison. Appending "/" prevents
	// "/radarr" from matching "/radarr-attacker".
	basePathPrefix := strings.TrimRight(baseU.Path, "/") + "/"

	// isSameBase reports whether u shares the same scheme, host, and path
	// prefix as the configured arr instance.
	isSameBase := func(u *url.URL) bool {
		return u.Scheme == baseU.Scheme &&
			u.Host == baseU.Host &&
			strings.HasPrefix(u.Path+"/", basePathPrefix)
	}

	// Use a client that strips the API key header on any redirect whose target
	// does not share the configured arr base origin and path. This allows
	// redirects to external CDNs to be followed while ensuring the API key is
	// exclusively sent to the configured arr instance.
	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !isSameBase(req.URL) {
				req.Header.Del("X-Api-Key")
			}

			return nil
		},
	}

	normalizedBase := baseU.Scheme + "://" + baseU.Host + strings.TrimRight(baseU.Path, "/")

	for _, img := range images {
		if img.CoverType != "poster" {
			continue
		}

		// Pre-fetch extension check: only accept JPEG and PNG.
		ext := strings.ToLower(img.Extension)
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			continue
		}

		imageURL := img.URL
		if imageURL == "" {
			imageURL = img.RemoteURL
		}

		if imageURL == "" {
			continue
		}

		// Relative paths are served by the arr instance; prepend the base URL.
		if strings.HasPrefix(imageURL, "/") {
			imageURL = normalizedBase + imageURL
		}

		imageU, err := url.Parse(imageURL)
		if err != nil {
			return nil, "", fmt.Errorf("parse image URL: %w", err)
		}

		// fetchCtx and cancel are managed explicitly (not via defer) to ensure
		// resources are released before continuing to the next candidate.
		fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)

		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, imageURL, nil)
		if err != nil {
			cancel()

			return nil, "", fmt.Errorf("build image request: %w", err)
		}

		// Only send the API key when fetching from the configured arr instance.
		// RemoteURL values may point to external CDNs and must not receive it.
		if isSameBase(imageU) {
			req.Header.Set("X-Api-Key", apiKey)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			cancel()

			return nil, "", fmt.Errorf("fetch poster image: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			cancel()

			continue
		}

		// Post-fetch Content-Type validation using the standard MIME parser.
		ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || (ct != MimeJPEG && ct != MimePNG) {
			_ = resp.Body.Close()

			cancel()

			continue
		}

		// Read one byte beyond the cap to detect responses that exceed the limit.
		// If the read returns more than MaxPosterBytes, skip this candidate rather
		// than silently truncating the image data.
		data, err := io.ReadAll(io.LimitReader(resp.Body, MaxPosterBytes+1))
		_ = resp.Body.Close()

		cancel()

		if err != nil {
			return nil, "", fmt.Errorf("read poster image: %w", err)
		}

		if len(data) > MaxPosterBytes {
			continue
		}

		return data, ct, nil
	}

	return nil, "", nil
}
