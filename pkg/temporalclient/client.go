// Package temporalclient builds a Temporal SDK client configured via the
// envconfig package, with two extensions on top:
//
//   - file:// API keys: when the resolved API key has the form
//     "file:///absolute/path", the path is read on every RPC via
//     [client.NewAPIKeyDynamicCredentials], so external rotators that update
//     the file in place are picked up without a process restart.
//   - Startup health check: [Dial] verifies the gRPC connection to the
//     frontend before returning. The underlying [client.Dial] is lazy and
//     would otherwise return a healthy-looking client even when the server
//     is unreachable.
package temporalclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
)

// apiKeyFileScheme and apiKeyFilePrefix together identify an API key value as
// a path to a file containing the key. Only the canonical empty-authority +
// absolute-path form ("file:///abs/path") is accepted; anything else starting
// with "file:" is rejected to catch typos like a single-slash "file:/path".
const (
	apiKeyFileScheme = "file:"
	apiKeyFilePrefix = "file://"
)

// healthCheckTimeout is how long Dial waits for the startup CheckHealth probe.
const healthCheckTimeout = 10 * time.Second

// Dial loads Temporal client options via envconfig, expands a file:// API key
// into dynamic credentials when present, and verifies the gRPC connection via
// CheckHealth before returning. Callers must close the returned client.
func Dial(ctx context.Context) (client.Client, error) {
	opts, err := buildOptions(ctx)
	if err != nil {
		return nil, err
	}

	c, err := client.Dial(opts)
	if err != nil {
		return nil, fmt.Errorf("dial Temporal: %w", err)
	}

	healthCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	if _, err := c.CheckHealth(healthCtx, &client.CheckHealthRequest{}); err != nil {
		c.Close()
		return nil, fmt.Errorf("temporal health check failed: %w", err)
	}

	return c, nil
}

// buildOptions loads client.Options via envconfig, replacing a file:// API key
// with dynamic credentials backed by os.ReadFile. A one-time validation read
// surfaces a misconfigured path at startup rather than at the first RPC. For
// the file-backed path, the loaded key's JWT claims (when applicable) are
// emitted at debug level so an operator can confirm which credential was
// loaded without exposing the signing material.
func buildOptions(ctx context.Context) (client.Options, error) {
	profile, err := envconfig.LoadClientConfigProfile(envconfig.LoadClientConfigProfileOptions{})
	if err != nil {
		return client.Options{}, fmt.Errorf("load temporal profile: %w", err)
	}

	apiKeyFile, err := extractAPIKeyFile(&profile)
	if err != nil {
		return client.Options{}, err
	}

	opts, err := profile.ToClientOptions(envconfig.ToClientOptionsRequest{})
	if err != nil {
		return client.Options{}, fmt.Errorf("build temporal client options: %w", err)
	}

	if apiKeyFile != "" {
		key, err := readAPIKeyFile(apiKeyFile)
		if err != nil {
			return client.Options{}, fmt.Errorf("validate api key file: %w", err)
		}

		logAPIKeyClaims(ctx, apiKeyFile, key)

		opts.Credentials = client.NewAPIKeyDynamicCredentials(func(context.Context) (string, error) {
			return readAPIKeyFile(apiKeyFile)
		})
	}

	return opts, nil
}

// readAPIKeyFile reads, trims, and non-empty-validates an API key file. Used
// both at startup (so a misconfigured path or empty file fails fast) and on
// every RPC via the dynamic-credentials callback (so a transient empty state
// during external rotation surfaces as a clear error rather than a silent
// empty bearer token).
func readAPIKeyFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read api key file %q: %w", path, err)
	}

	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("api key file %q is empty", path)
	}

	return key, nil
}

// logAPIKeyClaims attempts to decode a file-backed API key as a JWT and log
// its claims at debug level so an operator can confirm which credential was
// loaded — e.g. that sub/aud/exp match the expected service identity and
// lifetime. Inline keys aren't logged: they don't change at runtime, so the
// debug signal is far less useful than for the rotating file-backed path.
//
// Best-effort: silently no-ops for non-JWT keys (Temporal accepts any opaque
// bearer string). Only the decoded claims payload is logged — never the full
// token, header, or signature — so log records cannot be replayed as
// credentials. The signing material would still be needed to mint a new
// token, and that is never observable here.
func logAPIKeyClaims(ctx context.Context, path, apiKey string) {
	claims, ok := decodeJWTClaims(apiKey)
	if !ok {
		slog.DebugContext(ctx, "loaded temporal api key (not a JWT)", slog.String("path", path))

		return
	}

	slog.DebugContext(ctx, "loaded temporal api key", slog.String("path", path), slog.Any("claims", claims))
}

// decodeJWTClaims parses the claims segment of a JWS Compact-serialized JWT.
// It does not verify the signature; the caller has already chosen to trust
// the source of the token (env, TOML, or a mounted Secret file). Returns
// (claims, true) for a well-formed JWT, (nil, false) otherwise.
func decodeJWTClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}

	return claims, true
}

// extractAPIKeyFile detects a file:// API key, validates the form, and clears
// the inline value on the profile so envconfig's static-credentials path is
// bypassed. Returns the absolute file path, or "" when the key is inline.
//
// Any value starting with "file:" but not matching the canonical
// "file:///absolute/path" form is rejected. Treating non-canonical forms like
// "file:/path" as literal API keys would mask common typos as obscure auth
// failures at the server.
func extractAPIKeyFile(profile *envconfig.ClientConfigProfile) (string, error) {
	if !strings.HasPrefix(profile.APIKey, apiKeyFileScheme) {
		return "", nil
	}

	path, ok := strings.CutPrefix(profile.APIKey, apiKeyFilePrefix)
	if !ok {
		return "", fmt.Errorf("temporal api key %q: file URIs must use the canonical form (file:///path/to/key)", profile.APIKey)
	}

	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("temporal api key %q: file:// path must be absolute (file:///path/to/key)", profile.APIKey)
	}

	profile.APIKey = ""

	return path, nil
}
