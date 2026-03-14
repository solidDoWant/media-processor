package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_MissingToken covers AC5: given HATCHET_CLIENT_TOKEN is not set,
// when mediaprocessor-watcher starts, it exits non-zero with a descriptive error.
func TestRun_MissingToken(t *testing.T) {
	t.Setenv("HATCHET_CLIENT_TOKEN", "")

	// Provide a valid config so the error is specifically about the missing token.
	cfgPath := writeTempConfig(t, `watches:
  - path: /watch/movies
    workflow: MovieWorkflow
`)

	err := run(context.Background(), cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HATCHET_CLIENT_TOKEN")
}
