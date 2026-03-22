package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

// dispatchFunc submits a workflow run for the given absolute file path.
type dispatchFunc func(ctx context.Context, workflowName, filePath string) error

// NewScanWorkflow returns a Hatchet standalone task that scans all configured watch
// directories on the configured cron schedule and spawns a child workflow run for
// every file found, using the absolute file path as the idempotency key.
//
// Overlapping scans are dropped (CANCEL_NEWEST, max 1 concurrent run) so a slow
// scan does not pile up behind a cron backlog.
func NewScanWorkflow(client *hatchet.Client, cfg *Config) *hatchet.StandaloneTask {
	var maxRuns int32 = 1
	strategy := types.CancelNewest

	return client.NewStandaloneTask(
		"directory-scan",
		func(ctx hatchet.Context, _ struct{}) (struct{}, error) {
			dispatch := func(dispatchCtx context.Context, workflowName, filePath string) error {
				_, err := client.RunNoWait(
					dispatchCtx,
					workflowName,
					map[string]string{"file_path": filePath},
					hatchet.WithRunKey(filePath),
				)
				return err
			}
			return struct{}{}, scan(ctx, cfg, dispatch)
		},
		hatchet.WithWorkflowCron(cfg.CronSchedule),
		hatchet.WithWorkflowConcurrency(types.Concurrency{
			// Constant expression groups all scan runs under a single concurrency slot.
			Expression:    `"scan"`,
			MaxRuns:       &maxRuns,
			LimitStrategy: &strategy,
		}),
	)
}

// validateWatchDirs returns an error if any configured watch directory does not exist.
func validateWatchDirs(cfg *Config) error {
	for _, w := range cfg.Watches {
		if _, err := os.Stat(w.Path); err != nil {
			return fmt.Errorf("watch directory %q: %w", w.Path, err)
		}
	}
	return nil
}

// scan walks every configured watch directory recursively and calls dispatch for each
// file found. Files in subdirectories inherit the mapping of their watch root. Dispatch
// errors are logged as warnings so a single failed submission does not abort the scan.
func scan(ctx context.Context, cfg *Config, dispatch dispatchFunc) error {
	for _, w := range cfg.Watches {
		if err := filepath.WalkDir(w.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				log.Printf("warning: scan error at %q: %v", path, err)
				return nil
			}
			if d.IsDir() {
				return nil
			}
			absPath, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("resolve absolute path for %q: %w", path, err)
			}
			if dispatchErr := dispatch(ctx, w.Workflow, absPath); dispatchErr != nil {
				log.Printf("warning: dispatch workflow %q for %q: %v", w.Workflow, absPath, dispatchErr)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("walk directory %q: %w", w.Path, err)
		}
	}
	return nil
}
