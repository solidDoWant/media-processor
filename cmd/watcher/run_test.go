package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_MissingTaskQueue verifies that the watcher exits with a descriptive error
// when TEMPORAL_TASK_QUEUE is not set, even when the config file is valid.
func TestRun_MissingTaskQueue(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE", "")

	cfgPath := writeTempConfig(t, "watches: []")

	err := run(t.Context(), cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_TASK_QUEUE")
}
