package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_MissingToken verifies that the watcher exits with a descriptive error
// when HATCHET_CLIENT_TOKEN is not set, even when the config file is valid.
func TestRun_MissingToken(t *testing.T) {
	t.Setenv("HATCHET_CLIENT_TOKEN", "")

	cfgPath := writeTempConfig(t, "watches: []")

	err := run(t.Context(), cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HATCHET_CLIENT_TOKEN")
}
