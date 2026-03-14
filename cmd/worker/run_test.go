package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_MissingToken verifies that the worker exits with a descriptive error
// when HATCHET_CLIENT_TOKEN is not set.
func TestRun_MissingToken(t *testing.T) {
	t.Setenv("HATCHET_CLIENT_TOKEN", "")

	err := run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HATCHET_CLIENT_TOKEN")
}
