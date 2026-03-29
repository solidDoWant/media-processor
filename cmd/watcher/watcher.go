package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"github.com/solidDoWant/media-processor/pkg/medialib"
	"github.com/solidDoWant/media-processor/workflows/media"
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

// dispatchFunc submits a workflow run for the given absolute file path, media type, and mapping name.
type dispatchFunc func(ctx context.Context, filePath string, mediaType medialib.MediaType, mappingName string) error

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

// newScanInstruments registers all watcher scan instruments with the given MeterProvider.
func newScanInstruments(mp otelmetric.MeterProvider) (*scanInstruments, error) {
	meter := mp.Meter("github.com/solidDoWant/media-processor/cmd/watcher")

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
			dispatch := func(dispatchCtx context.Context, filePath string, mediaType medialib.MediaType, mappingName string) error {
				_, err := client.RunNoWait(
					dispatchCtx,
					media.MediaWorkflowName,
					map[string]string{
						"file_path":    filePath,
						"media_type":   string(mediaType),
						"mapping_name": mappingName,
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
		info, err := os.Stat(w.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("watch directory %q: %w", w.Path, err))
			continue
		}

		if !info.IsDir() {
			errs = append(errs, fmt.Errorf("watch path %q is not a directory", w.Path))
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

		mappingAttr := otelmetric.WithAttributes(
			mappingNameAttr(w.Name),
		)

		var mappingErrs []error
		start := time.Now()

		if err := filepath.WalkDir(w.Path, func(path string, d fs.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}

			if err != nil {
				mappingErrs = append(mappingErrs, fmt.Errorf("scan error at %q: %w", path, err))
				return nil
			}

			if d.IsDir() {
				return nil
			}

			absPath, err := filepath.Abs(path)
			if err != nil {
				mappingErrs = append(mappingErrs, fmt.Errorf("resolve absolute path for %q: %w", path, err))
				return nil
			}

			instruments.filesDiscoveredTotal.Add(ctx, 1,
				otelmetric.WithAttributes(mappingNameAttr(w.Name), mediaTypeAttr(w.MediaType)))

			if dispatchErr := dispatch(ctx, absPath, w.MediaType, w.Name); dispatchErr != nil {
				mappingErrs = append(mappingErrs, fmt.Errorf("dispatch workflow for %q (media type %v): %w", absPath, w.MediaType, dispatchErr))
				instruments.dispatchErrorsTotal.Add(ctx, 1,
					otelmetric.WithAttributes(mappingNameAttr(w.Name), mediaTypeAttr(w.MediaType)))
			} else {
				instruments.dispatchesTotal.Add(ctx, 1,
					otelmetric.WithAttributes(mappingNameAttr(w.Name), mediaTypeAttr(w.MediaType)))
			}

			return nil
		}); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			mappingErrs = append(mappingErrs, fmt.Errorf("walk directory %q: %w", w.Path, err))
		}

		duration := time.Since(start).Seconds()
		instruments.scanDuration.Record(ctx, duration, mappingAttr)

		if len(mappingErrs) == 0 {
			instruments.scansTotal.Add(ctx, 1,
				otelmetric.WithAttributes(mappingNameAttr(w.Name), statusAttr("success")))
			instruments.lastSuccessfulScan.Record(ctx, float64(time.Now().Unix()), mappingAttr)
		} else {
			instruments.scansTotal.Add(ctx, 1,
				otelmetric.WithAttributes(mappingNameAttr(w.Name), statusAttr("error")))
			errs = append(errs, mappingErrs...)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("directory scan completed with errors: %w", err)
	}

	return nil
}
