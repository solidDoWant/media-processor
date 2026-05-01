//go:build integration

package temporalclient_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/media-processor/pkg/temporalclient"
)

// requireTemporalAddress ensures the test runner has a Temporal server reachable
// at TEMPORAL_ADDRESS. The make test-integration target arranges this via
// temporal-up before invoking the suite.
func requireTemporalAddress(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// isolateTemporalConfig points TEMPORAL_CONFIG_FILE at a fresh empty TOML file
// so envconfig does not pick up a stray ~/.config/temporalio/temporal.toml on
// the host. Each test calling this gets its own deterministic baseline.
func isolateTemporalConfig(t *testing.T) {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")

	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")
	t.
}

func TestDialHappyPath(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	c, err := temporalclient.Dial(t.Context())
	require.NoError(t, err)

	defer c.Close()

	// Sanity-check the client beyond Dial's internal CheckHealth: a fresh
	// CheckHealth round-trip from the test confirms the returned client is
	// usable for outbound RPCs, not just the one Dial issued.
	_, err = c.CheckHealth(t.Context(), &client.CheckHealthRequest{})
	assert.NoError(t, err)
}

func TestDialFailsWhenServerUnreachable(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	// Port 1 is reserved (tcpmux) and not bound on dev workstations or in CI.
	// Depending on the underlying gRPC behaviour, the failure surfaces either
	// at client.Dial (immediate connection refused) or at the startup
	// CheckHealth (lazy dial that the call exercises) — both are legitimate
	// outcomes for the user-visible contract that Dial returns an error when
	// the frontend is unreachable, so we assert on neither prefix specifically.
	t.Setenv("TEMPORAL_ADDRESS", "127.0.0.1:1")

	_, err := temporalclient.Dial(t.Context())
	require.Error(t, err)
	// Accept either error path: "dial Temporal:" wraps an immediate connection
	// failure, "temporal health check failed:" wraps a CheckHealth timeout
	// after a lazy gRPC dial. A case-insensitive substring matches both.
	assert.Contains(t, strings.ToLower(err.Error()), "temporal")
}

func TestDialWithFileBackedAPIKey(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	keyFile := filepath.Join(t.TempDir(), "api-key")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key-value\n"), 0o600))

	t.Setenv("TEMPORAL_API_KEY", "file://"+keyFile)
	// Setting an API key auto-enables TLS in the SDK; force it back off so we
	// can dial the plaintext dev frontend. The header still rides on the gRPC
	// call — the dev server just doesn't enforce it.
	t.Setenv("TEMPORAL_TLS", "false")

	c, err := temporalclient.Dial(t.Context())
	require.NoError(t, err)

	defer c.Close()

	_, err = c.CheckHealth(t.Context(), &client.CheckHealthRequest{})
	assert.NoError(t, err)
}

func TestDialFailsWhenAPIKeyFileMissing(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist")

	t.Setenv("TEMPORAL_API_KEY", "file://"+missing)
	t.Setenv("TEMPORAL_TLS", "false")

	// The misconfiguration surfaces during Dial's CheckHealth: the dynamic-
	// credentials callback is invoked by the gRPC interceptor, fails its
	// os.ReadFile, and that error propagates back through CheckHealth.
	_, err := temporalclient.Dial(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "read api key file")
}

func TestDialFailsWhenAPIKeyFilePathRelative(t *testing.T) {
	requireTemporalAddress(t)
	isolateTemporalConfig(t)

	t.Setenv("TEMPORAL_API_KEY", "file://relative/path")
	t.Setenv("TEMPORAL_TLS", "false")

	_, err := temporalclient.Dial(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "must be absolute")
}
