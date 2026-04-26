package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
)

func mappingNameAttr(name string) attribute.KeyValue {
	return attribute.String("mapping_name", name)
}

func mediaTypeAttr(mt medialib.MediaType) attribute.KeyValue {
	return attribute.String("media_type", string(mt))
}

func statusAttr(status string) attribute.KeyValue {
	return attribute.String("status", status)
}

// dispatchFunc submits a workflow run for the given absolute file path, media type, mapping name,
// whether to preserve the source file after processing, the watch root directory, whether to
// retain empty parent directories after source-file deletion, the absolute output directory path,
// and the arr-side remote output path prefix (empty means no translation).
type dispatchFunc func(ctx context.Context, filePath string, mediaType medialib.MediaType, mappingName string, preserveSource bool, watchRoot string, retainEmptyDirs bool, outputPath string, outputRemotePath string) error

// scanInstruments holds all OTel instruments used during scan. Instruments are registered
// once at startup and reused across every scan invocation.
type scanInstruments struct {
	scansTotal           otelmetric.Int64Counter
	scanDuration         otelmetric.Float64Histogram
	lastSuccessfulScan   otelmetric.Float64Gauge
	filesDiscoveredTotal otelmetric.Int64Counter
	dispatchesTotal      otelmetric.Int64Counter
	dispatchErrorsTotal  otelmetric.Int64Counter
}

// meterName is the OTel instrumentation scope name for this package.
const meterName = "github.com/solidDoWant/media-processor/cmd/watcher"

// scan status label values used with watcher_scans_total.
const (
	scanStatusSuccess = "success"
	scanStatusError   = "error"
)

// newScanInstruments registers all watcher scan instruments with the given MeterProvider.
func newScanInstruments(mp otelmetric.MeterProvider) (*scanInstruments, error) {
	meter := mp.Meter(meterName)

	scansTotal, err := meter.Int64Counter("watcher_scans_total",
		otelmetric.WithDescription("Total number of per-mapping directory scans completed."))
	if err != nil {
		return nil, fmt.Errorf("create watcher_scans_total: %w", err)
	}

	scanDuration, err := meter.Float64Histogram("watcher_scan_duration_seconds",
		otelmetric.WithDescription("Wall-clock duration of each per-mapping directory walk in seconds."),
		otelmetric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("create watcher_scan_duration_seconds: %w", err)
	}

	lastSuccessfulScan, err := meter.Float64Gauge("watcher_last_successful_scan_unix_seconds",
		otelmetric.WithDescription("Unix timestamp of the most recent successful per-mapping scan."),
		otelmetric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("create watcher_last_successful_scan_unix_seconds: %w", err)
	}

	filesDiscoveredTotal, err := meter.Int64Counter("watcher_files_discovered_total",
		otelmetric.WithDescription("Total number of files found during directory scans."))
	if err != nil {
		return nil, fmt.Errorf("create watcher_files_discovered_total: %w", err)
	}

	dispatchesTotal, err := meter.Int64Counter("watcher_dispatches_total",
		otelmetric.WithDescription("Total number of workflow dispatches successfully submitted to Hatchet."))
	if err != nil {
		return nil, fmt.Errorf("create watcher_dispatches_total: %w", err)
	}

	dispatchErrorsTotal, err := meter.Int64Counter("watcher_dispatch_errors_total",
		otelmetric.WithDescription("Total number of workflow dispatch failures."))
	if err != nil {
		return nil, fmt.Errorf("create watcher_dispatch_errors_total: %w", err)
	}

	return &scanInstruments{
		scansTotal:           scansTotal,
		scanDuration:         scanDuration,
		lastSuccessfulScan:   lastSuccessfulScan,
		filesDiscoveredTotal: filesDiscoveredTotal,
		dispatchesTotal:      dispatchesTotal,
		dispatchErrorsTotal:  dispatchErrorsTotal,
	}, nil
}

// NewScanWorkflow returns a Hatchet standalone task that scans all configured watch
// directories on the configured cron schedule and spawns a child workflow run for
// every file found, using the absolute file path as the idempotency key.
//
// Overlapping scans are dropped (CANCEL_NEWEST, max 1 concurrent run) so a slow
// scan does not pile up behind a cron backlog.
func NewScanWorkflow(client *hatchet.Client, cfg *Config, mp otelmetric.MeterProvider) (*hatchet.StandaloneTask, error) {
	maxRuns := int32(1)
	strategy := types.CancelNewest

	instruments, err := newScanInstruments(mp)
	if err != nil {
		return nil, fmt.Errorf("register scan metrics: %w", err)
	}

	task := client.NewStandaloneTask(
		"directory-scan",
		func(ctx hatchet.Context, _ struct{}) (struct{}, error) {
			dispatch := func(dispatchCtx context.Context, filePath string, mediaType medialib.MediaType, mappingName string, preserveSource bool, watchRoot string, retainEmptyDirs bool, outputPath string, outputRemotePath string) error {
				_, err := client.RunNoWait(
					dispatchCtx,
					mediatypes.MediaWorkflowName,
					mediatypes.MediaInput{
						FilePath:               filePath,
						MediaType:              mediaType,
						MappingName:            mappingName,
						PreserveSource:         preserveSource,
						WatchRoot:              watchRoot,
						RetainEmptyDirectories: retainEmptyDirs,
						OutputPath:             outputPath,
						OutputRemotePath:       outputRemotePath,
					},
					hatchet.WithRunKey(filePath),
				)

				return err
			}

			return struct{}{}, scan(ctx, cfg, instruments, dispatch)
		},
		hatchet.WithWorkflowCron(string(cfg.CronSchedule)),
		hatchet.WithWorkflowConcurrency(types.Concurrency{
			// Constant expression groups all scan runs under a single concurrency slot.
			Expression:    `"scan"`,
			MaxRuns:       &maxRuns,
			LimitStrategy: &strategy,
		}),
	)

	return task, nil
}

// validateWatchDirs returns an error listing every configured watch directory that does
// not exist or is not a directory, so the operator sees all problems at once rather than
// fixing them one at a time.
func validateWatchDirs(cfg *Config) error {
	errs := make([]error, 0, len(cfg.Watches))

	for _, w := range cfg.Watches {
		info, err := os.Stat(w.WatchedPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("watch directory %q: %w", w.WatchedPath, err))
			continue
		}

		if !info.IsDir() {
			errs = append(errs, fmt.Errorf("watch path %q is not a directory", w.WatchedPath))
		}
	}

	return errors.Join(errs...)
}

// scan walks every configured watch directory recursively and calls dispatch for each
// file found. Files in subdirectories inherit the mapping of their watch root. All
// per-file errors (access errors and dispatch errors) are collected and returned as an
// aggregate so a single failure does not abort the scan. The walk respects ctx
// cancellation and returns immediately on shutdown.
func scan(ctx context.Context, cfg *Config, instruments *scanInstruments, dispatch dispatchFunc) error {
	var errs []error

	for _, w := range cfg.Watches {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Precompute attribute option sets once per watch entry to avoid repeated
		// allocation inside the WalkDir callback (one set per file found).
		mappingOpt := otelmetric.WithAttributes(mappingNameAttr(w.Name))
		fileOpt := otelmetric.WithAttributes(mappingNameAttr(w.Name), mediaTypeAttr(w.MediaType))
		successOpt := otelmetric.WithAttributes(mappingNameAttr(w.Name), statusAttr(scanStatusSuccess))
		errorOpt := otelmetric.WithAttributes(mappingNameAttr(w.Name), statusAttr(scanStatusError))

		var (
			mappingErrs     []error
			filesDiscovered int
			jobsSubmitted   int
		)

		// Normalise the watch path to an absolute path once per entry so that watchRoot
		// is always comparable to the absolute file paths produced inside the walk callback.
		absWatchRoot, err := filepath.Abs(w.WatchedPath)
		if err != nil {
			mappingErrs = append(mappingErrs, fmt.Errorf("resolve absolute path for watch directory %q: %w", w.WatchedPath, err))
			errs = append(errs, mappingErrs...)

			continue
		}

		trimmedOutputPath := strings.TrimSpace(w.Output.Path)
		if trimmedOutputPath == "" {
			mappingErrs = append(mappingErrs, fmt.Errorf("output.path is blank or whitespace-only for watch %q (watched path %q)", w.Name, w.WatchedPath))
			errs = append(errs, mappingErrs...)

			continue
		}

		absOutputPath, err := filepath.Abs(trimmedOutputPath)
		if err != nil {
			mappingErrs = append(mappingErrs, fmt.Errorf("resolve absolute path for output directory %q: %w", w.Output.Path, err))
			errs = append(errs, mappingErrs...)

			continue
		}

		outputRemotePath := strings.TrimSpace(w.Output.RemotePath)

		start := time.Now()

		if err := filepath.WalkDir(absWatchRoot, func(path string, d fs.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}

			if err != nil {
				mappingErrs = append(mappingErrs, fmt.Errorf("scan error at %q: %w", path, err))
				return nil
			}

			for _, pattern := range w.IgnorePatterns {
				if pattern.MatchString(path) {
					if d.IsDir() {
						return filepath.SkipDir
					}

					return nil
				}
			}

			if d.IsDir() {
				return nil
			}

			// Skip sentinel files (hidden .BASENAME.done markers).
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".done") {
				return nil
			}

			// Skip files whose processing sentinel already exists on disk.
			sentinelPath := filepath.Join(filepath.Dir(path), "."+base+".done")
			if _, statErr := os.Stat(sentinelPath); statErr == nil {
				return nil
			} else if !os.IsNotExist(statErr) {
				mappingErrs = append(mappingErrs, fmt.Errorf("scan sentinel %q for %q: %w", sentinelPath, path, statErr))

				return nil
			}

			instruments.filesDiscoveredTotal.Add(ctx, 1, fileOpt)

			filesDiscovered++

			if dispatchErr := dispatch(ctx, path, w.MediaType, w.Name, w.PreserveSource, absWatchRoot, w.RetainEmptyDirectories, absOutputPath, outputRemotePath); dispatchErr != nil {
				mappingErrs = append(mappingErrs, fmt.Errorf("dispatch workflow for %q (media type %v): %w", path, w.MediaType, dispatchErr))

				instruments.dispatchErrorsTotal.Add(ctx, 1, fileOpt)
			} else {
				instruments.dispatchesTotal.Add(ctx, 1, fileOpt)

				jobsSubmitted++

				slog.InfoContext(ctx, "dispatched workflow", slog.String("file", path), slog.String("watch", w.Name))
			}

			return nil
		}); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			mappingErrs = append(mappingErrs, fmt.Errorf("walk directory %q: %w", w.WatchedPath, err))
		}

		slog.InfoContext(ctx, "scan complete",
			slog.String("watch", w.Name),
			slog.Int("files_discovered", filesDiscovered),
			slog.Int("jobs_submitted", jobsSubmitted),
		)

		duration := time.Since(start).Seconds()
		instruments.scanDuration.Record(ctx, duration, mappingOpt)

		if len(mappingErrs) == 0 {
			instruments.scansTotal.Add(ctx, 1, successOpt)
			instruments.lastSuccessfulScan.Record(ctx, float64(time.Now().Unix()), mappingOpt)
		} else {
			instruments.scansTotal.Add(ctx, 1, errorOpt)

			errs = append(errs, mappingErrs...)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("directory scan completed with errors: %w", err)
	}

	return nil
}
