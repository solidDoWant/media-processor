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
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
)

// dialTestClient creates a Temporal client for integration tests using TEMPORAL_ADDRESS
// and TEMPORAL_NAMESPACE env vars.
func dialTestClient(t *testing.T) client.Client {
	t.Helper()

	c, err := client.Dial(client.Options{
		HostPort:  os.Getenv("TEMPORAL_ADDRESS"),
		Namespace: os.Getenv("TEMPORAL_NAMESPACE"),
	})
	require.NoError(t, err, "dial Temporal")

	t.Cleanup(c.Close)

	return c
}

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

	c := dialTestClient(t)

	dispatch := newTemporalDispatch(c, taskQueue, false)

	filePath := filepath.Join(t.TempDir(), "movie.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	input := mediatypes.MediaInput{
		FilePath:    filePath,
		MediaType:   medialib.MovieType,
		MappingName: "movies",
		WatchRoot:   "/watch",
		OutputPath:  "/out",
	}

	require.NoError(t,
		dispatch(t.Context(), input),
		"first dispatch should succeed",
	)

	err := dispatch(t.Context(), input)
	require.ErrorIs(t, err, errWorkflowAlreadyStarted, "second dispatch should be deduplicated")

	wfID := workflowID(input)

	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), wfID, "", "test cleanup")
	})

	desc, err := c.DescribeWorkflowExecution(t.Context(), wfID, "")
	require.NoError(t, err)
	assert.NotNil(t, desc.WorkflowExecutionInfo, "exactly one workflow execution should exist for the file")
}

// TestMediaWorkflow_MemoAttached verifies that a dispatched media workflow has a
// Memo with all five expected fields (file path, title, media type, mapping name,
// watch root) present and non-empty.
func TestMediaWorkflow_MemoAttached(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	taskQueue := "watcher-memo-" + time.Now().Format("150405.000000000")
	c := dialTestClient(t)

	filePath := filepath.Join(t.TempDir(), "movie.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	input := mediatypes.MediaInput{
		FilePath:    filePath,
		MediaType:   medialib.MovieType,
		MappingName: "movies",
		WatchRoot:   "/watch",
		OutputPath:  "/out",
	}

	dispatch := newTemporalDispatch(c, taskQueue, false)
	require.NoError(t, dispatch(t.Context(), input), "dispatch should succeed")

	wfID := workflowID(input)

	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), wfID, "", "test cleanup")
	})

	desc, err := c.DescribeWorkflowExecution(t.Context(), wfID, "")
	require.NoError(t, err)

	memo := desc.WorkflowExecutionInfo.GetMemo()
	require.NotNil(t, memo, "Memo should be attached to the workflow execution")

	expectedKeys := []string{"MediaFilePath", "MediaTitle", "MediaType", "MediaMappingName", "MediaWatchRoot"}
	for _, key := range expectedKeys {
		assert.Contains(t, memo.GetFields(), key, "Memo should contain key %q", key)
	}
}

// TestMediaWorkflow_SearchAttributesQueryable verifies that a dispatched media
// workflow can be found by querying the Temporal visibility API with a custom
// search attribute filter (MediaMappingName = "movies").
//
// Requires TEMPORAL_ADDRESS to be set and the custom search attributes to be
// pre-registered in the Temporal namespace. Set TEMPORAL_SEARCH_ATTRIBUTES_REGISTERED=true
// to enable this test; leave it unset to skip when advanced visibility is unavailable.
func TestMediaWorkflow_SearchAttributesQueryable(t *testing.T) {
	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; bring up a Temporal server first")
	}

	if os.Getenv("TEMPORAL_SEARCH_ATTRIBUTES_REGISTERED") != "true" {
		t.Skip("TEMPORAL_SEARCH_ATTRIBUTES_REGISTERED not set to 'true'; register custom search attributes and set this var to test SA visibility")
	}

	taskQueue := "watcher-sa-query-" + time.Now().Format("150405.000000000")
	c := dialTestClient(t)

	filePath := filepath.Join(t.TempDir(), "query-test.mkv")
	require.NoError(t, os.WriteFile(filePath, []byte{}, 0o600))

	mappingName := "sa-query-test-" + time.Now().Format("150405.000000000")
	input := mediatypes.MediaInput{
		FilePath:    filePath,
		MediaType:   medialib.MovieType,
		MappingName: mappingName,
		WatchRoot:   "/watch",
		OutputPath:  "/out",
	}

	dispatch := newTemporalDispatch(c, taskQueue, true)
	require.NoError(t, dispatch(t.Context(), input), "dispatch with search attributes should succeed")

	wfID := workflowID(input)

	t.Cleanup(func() {
		_ = c.TerminateWorkflow(context.Background(), wfID, "", "test cleanup")
	})

	// Temporal visibility indexing may be slightly delayed; retry the query briefly.
	query := `MediaMappingName = "` + mappingName + `"`

	var found bool

	const maxAttempts = 10

	const retryInterval = 500 * time.Millisecond

	for range maxAttempts {
		resp, err := c.ListWorkflow(t.Context(), &workflowservice.ListWorkflowExecutionsRequest{
			Query: query,
		})
		require.NoError(t, err)

		if len(resp.GetExecutions()) > 0 {
			found = true
			break
		}

		time.Sleep(retryInterval)
	}

	assert.True(t, found, "workflow should be findable via MediaMappingName search attribute query %q", query)
}
