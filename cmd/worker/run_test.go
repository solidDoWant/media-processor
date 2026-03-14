package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_MissingToken covers AC5: given HATCHET_CLIENT_TOKEN is not set,
// when mediaprocessor-worker starts, it exits non-zero with a descriptive error.
func TestRun_MissingToken(t *testing.T) {
	t.Setenv("HATCHET_CLIENT_TOKEN", "")

	err := run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HATCHET_CLIENT_TOKEN")
}
