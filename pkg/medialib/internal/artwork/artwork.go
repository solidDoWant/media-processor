// Package artwork provides shared utilities for fetching poster images from
// arr-style media library services (Radarr, Sonarr).
package artwork

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
	MaxPosterBytes = 10 * 1024 * 1024 // 10 MiB
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

	// normalizedBaseURL is the base URL without trailing slash, query, or
	// fragment — used to expand relative image paths.
	normalizedBaseURL := &url.URL{Scheme: baseU.Scheme, Host: baseU.Host, Path: strings.TrimRight(baseU.Path, "/")}

	for _, img := range images {
		data, mimeType, err := fetchPosterCandidate(ctx, img, normalizedBaseURL, isSameBase, httpClient, apiKey)
		if err != nil {
			slog.WarnContext(ctx, "skipping poster candidate: fetch error", "error", err)

			continue
		}

		if data != nil {
			return data, mimeType, nil
		}
	}

	return nil, "", nil
}

// fetchPosterCandidate attempts to fetch a single poster image candidate.
// Returns non-nil bytes and a MIME type on success.
// Returns nil bytes (no error) when the candidate should be skipped (unsupported
// extension, empty URL, non-200 response, unsupported content type, or oversized body).
// Returns a non-nil error for hard I/O failures (URL parse error, request build
// error, network error, or body read error); callers should log and skip the candidate.
func fetchPosterCandidate(ctx context.Context, img *starr.Image, normalizedBaseURL *url.URL, isSameBase func(*url.URL) bool, httpClient *http.Client, apiKey string) ([]byte, string, error) {
	if img.CoverType != "poster" {
		return nil, "", nil
	}

	// Pre-fetch extension check: only accept JPEG and PNG.
	ext := strings.ToLower(img.Extension)
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		slog.WarnContext(ctx, "skipping poster candidate: unsupported extension", "extension", img.Extension)

		return nil, "", nil
	}

	imageURL := img.URL
	if imageURL == "" {
		imageURL = img.RemoteURL
	}

	if imageURL == "" {
		slog.WarnContext(ctx, "skipping poster candidate: no URL or RemoteURL set")

		return nil, "", nil
	}

	// Relative paths are served by the arr instance; prepend the base URL.
	if strings.HasPrefix(imageURL, "/") {
		imageURL = normalizedBaseURL.String() + imageURL
	}

	imageU, err := url.Parse(imageURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse image URL: %w", err)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build image request: %w", err)
	}

	// Only send the API key when fetching from the configured arr instance.
	// RemoteURL values may point to external CDNs and must not receive it.
	if isSameBase(imageU) {
		req.Header.Set("X-Api-Key", apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch poster image: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "skipping poster candidate: non-200 response", "status", resp.StatusCode, "url", imageURL)

		return nil, "", nil
	}

	// Post-fetch Content-Type validation using the standard MIME parser.
	mimeType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mimeType != MimeJPEG && mimeType != MimePNG) {
		slog.WarnContext(ctx, "skipping poster candidate: unsupported content type", "content_type", resp.Header.Get("Content-Type"), "url", imageURL)

		return nil, "", nil
	}

	// Read one byte beyond the cap to detect responses that exceed the limit.
	// If the read returns more than MaxPosterBytes, skip this candidate rather
	// than silently truncating the image data.
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxPosterBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read poster image: %w", err)
	}

	if len(data) > MaxPosterBytes {
		slog.WarnContext(ctx, "skipping poster candidate: response exceeds size limit", "limit_bytes", MaxPosterBytes, "url", imageURL)

		return nil, "", nil
	}

	return data, mimeType, nil
}
