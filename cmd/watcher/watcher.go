package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	maxRuns := int32(1)
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

// validateWatchDirs returns an error listing every configured watch directory that does
// not exist, so the operator sees all problems at once rather than fixing them one at a time.
func validateWatchDirs(cfg *Config) error {
	errs := make([]error, 0, len(cfg.Watches))

	for _, w := range cfg.Watches {
		if _, err := os.Stat(w.Path); err != nil {
			errs = append(errs, fmt.Errorf("watch directory %q: %w", w.Path, err))
		}
	}

	return errors.Join(errs...)
}

// scan walks every configured watch directory recursively and calls dispatch for each
// file found. Files in subdirectories inherit the mapping of their watch root. All
// per-file errors (access errors and dispatch errors) are collected and returned as an
// aggregate so a single failure does not abort the scan.
func scan(ctx context.Context, cfg *Config, dispatch dispatchFunc) error {
	var errs []error

	for _, w := range cfg.Watches {
		if err := filepath.WalkDir(w.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, fmt.Errorf("scan error at %q: %w", path, err))
				return nil
			}

			if d.IsDir() {
				return nil
			}

			absPath, err := filepath.Abs(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("resolve absolute path for %q: %w", path, err))
				return nil
			}

			if dispatchErr := dispatch(ctx, w.Workflow, absPath); dispatchErr != nil {
				errs = append(errs, fmt.Errorf("dispatch workflow %q for %q: %w", w.Workflow, absPath, dispatchErr))
			}

			return nil
		}); err != nil {
			errs = append(errs, fmt.Errorf("walk directory %q: %w", w.Path, err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("directory scan completed with errors: %w", err)
	}

	return nil
}
