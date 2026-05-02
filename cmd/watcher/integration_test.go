//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// TestWatcher_ConnectsToTemporal verifies that the watcher successfully connects to a
// running Temporal server given a valid config file.
//
// Requires a running Temporal server. Set TEMPORAL_ADDRESS before executing.
func TestWatcher_ConnectsToTemporal(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	if os.Getenv("TEMPORAL_TASK_QUEUE") == "" {
		t.Setenv("TEMPORAL_TASK_QUEUE", "watcher-integration-"+time.Now().Format("150405.000000000"))
	}

	cfgPath := writeTempConfig(t, "scanInterval: 100ms\nwatches: []")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- run(ctx, cfgPath)
	}()

	// If the watcher fails to connect, run() returns quickly with an error.
	// If it connects successfully, it blocks until the context is cancelled.
	const connectionWindow = 10 * time.Second

	timer := time.NewTimer(connectionWindow)
	defer timer.Stop()

	select {
	case err := <-done:
		require.NoError(t, err, "watcher exited before context was cancelled — connection likely failed")
	case <-timer.C:
		// Watcher is still running after the window — connection succeeded.
	}

	cancel()
	require.NoError(t, <-done, "watcher should shut down cleanly")
}

// TestMultiWatcherDedup_OnlyOneWorkflowPerFile verifies AC #3: when two watcher
// instances submit ExecuteWorkflow for the same file, exactly one workflow
// execution is created and the second submission is rejected by Temporal's
// running-duplicate handling.
//
// The test runs against a task queue with no registered workers. Submitted
// workflows therefore sit in "Running" state waiting for a workflow task,
// which is enough to make Temporal reject duplicate-ID submissions.
func TestMultiWatcherDedup_OnlyOneWorkflowPerFile(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	taskQueue := "watcher-dedup-" + time.Now().Format("150405.000000000")

	c, err := client.Dial(client.Options{
		HostPort:  os.Getenv("TEMPORAL_ADDRESS"),
		Namespace: os.Getenv("TEMPORAL_NAMESPACE"),
	})
	require.NoError(t, err, "dial Temporal")

	t.Cleanup(c.Close)

	dispatch := newTemporalDispatch(c, taskQueue)

	filePath := filepath.Join(t.TempDir(), "movie.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	require.NoError(t,
		dispatch(t.Context(), filePath, medialib.MovieType, "movies", false, "/watch", false, "/out", ""),
		"first dispatch should succeed",
	)

	err = dispatch(t.Context(), filePath, medialib.MovieType, "movies", false, "/watch", false, "/out", "")
	require.ErrorIs(t, err, errWorkflowAlreadyStarted, "second dispatch should be deduplicated")

	wfID := workflowID(filePath)

	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), wfID, "", "test cleanup")
	})

	desc, err := c.DescribeWorkflowExecution(t.Context(), wfID, "")
	require.NoError(t, err)
	assert.NotNil(t, desc.WorkflowExecutionInfo, "exactly one workflow execution should exist for the file")
}
