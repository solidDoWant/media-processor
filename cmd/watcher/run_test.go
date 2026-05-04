package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_DefaultsTaskQueueWhenUnset verifies that an unset TEMPORAL_TASK_QUEUE
// no longer fails the watcher at startup — the documented default is applied
// instead. The test forces an early failure further down the boot sequence
// (an invalid HEALTH_ADDR) so it does not need a running Temporal server.
func TestRun_DefaultsTaskQueueWhenUnset(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE", "")
	t.Setenv("HEALTH_ADDR", "not-a-valid:listen:address")

	cfgPath := writeTempConfig(t, "watches: []")

	err := run(t.Context(), cfgPath)
	require.Error(t, err, "the invalid HEALTH_ADDR should fail later in the boot sequence")
	assert.NotContains(t, err.Error(), "TEMPORAL_TASK_QUEUE", "missing TEMPORAL_TASK_QUEUE must no longer fail run() — it should fall back to the default")
}
