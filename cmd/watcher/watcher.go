package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	mediatypes "github.com/solidDoWant/media-processor/workflows/media/types"
)

// errWorkflowAlreadyStarted is returned by dispatch when Temporal rejects an
// ExecuteWorkflow call because a workflow with the same WorkflowID is already
// running. This is the expected outcome of multi-watcher dedup, so the scan
// loop counts it as neither a dispatch nor a dispatch error.
var errWorkflowAlreadyStarted = errors.New("workflow already started")

// dispatchFunc submits a workflow run for the given media input.
type dispatchFunc func(ctx context.Context, input mediatypes.MediaInput) error

// scanInstruments holds all Prometheus collectors used during scan. Collectors
// are registered once at startup and reused across every scan invocation.
type scanInstruments struct {
	scansTotal           *prometheus.CounterVec
	scanDuration         *prometheus.HistogramVec
	lastSuccessfulScan   *prometheus.GaugeVec
	filesDiscoveredTotal *prometheus.CounterVec
	dispatchesTotal      *prometheus.CounterVec
	dispatchErrorsTotal  *prometheus.CounterVec
}

// scan status label values used with watcher_scans_total.
const (
	scanStatusSuccess = "success"
	scanStatusError   = "error"
)

// scanDurationBuckets bound the per-mapping walk durations expected in
// practice (sub-second through a couple of minutes for very large libraries).
var scanDurationBuckets = []float64{0.001, 0.01, 0.1, 0.5, 1, 5, 30, 60, 120}

// newScanInstruments registers all watcher scan collectors with reg.
func newScanInstruments(reg prometheus.Registerer) (*scanInstruments, error) {
	scansTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "watcher_scans_total",
		Help: "Total number of per-mapping directory scans completed.",
	}, []string{"mapping_name", "status"})
	if err := reg.Register(scansTotal); err != nil {
		return nil, fmt.Errorf("register watcher_scans_total: %w", err)
	}

	scanDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "watcher_scan_duration_seconds",
		Help:    "Wall-clock duration of each per-mapping directory walk in seconds.",
		Buckets: scanDurationBuckets,
	}, []string{"mapping_name"})
	if err := reg.Register(scanDuration); err != nil {
		return nil, fmt.Errorf("register watcher_scan_duration_seconds: %w", err)
	}

	lastSuccessfulScan := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "watcher_last_successful_scan_unix_seconds",
		Help: "Unix timestamp of the most recent successful per-mapping scan.",
	}, []string{"mapping_name"})
	if err := reg.Register(lastSuccessfulScan); err != nil {
		return nil, fmt.Errorf("register watcher_last_successful_scan_unix_seconds: %w", err)
	}

	filesDiscoveredTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "watcher_files_discovered_total",
		Help: "Total number of files found during directory scans.",
	}, []string{"mapping_name", "media_type"})
	if err := reg.Register(filesDiscoveredTotal); err != nil {
		return nil, fmt.Errorf("register watcher_files_discovered_total: %w", err)
	}

	dispatchesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "watcher_dispatches_total",
		Help: "Total number of workflow dispatches successfully submitted.",
	}, []string{"mapping_name", "media_type"})
	if err := reg.Register(dispatchesTotal); err != nil {
		return nil, fmt.Errorf("register watcher_dispatches_total: %w", err)
	}

	dispatchErrorsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "watcher_dispatch_errors_total",
		Help: "Total number of workflow dispatch failures.",
	}, []string{"mapping_name", "media_type"})
	if err := reg.Register(dispatchErrorsTotal); err != nil {
		return nil, fmt.Errorf("register watcher_dispatch_errors_total: %w", err)
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

// workflowIDMaxLen is Temporal's hard cap on WorkflowID length. workflowID()
// truncates the basename segment to keep the overall ID within this bound.
const workflowIDMaxLen = 1000

// workflowIDShortHashLen is the number of hex characters (48 bits) taken from
// the head of sha256(absFilePath) for the trailing hash segment. Enough
// collision resistance for a per-host watcher while keeping the ID short and
// scannable in the Temporal Web UI.
const workflowIDShortHashLen = 12

// workflowID derives a human-readable, deterministic Temporal WorkflowID for a
// media file. The format is "{mappingName}-{basename}-{shortHash}" where
// mappingName and basename are sanitized so only [A-Za-z0-9._-] survive
// (anything else replaced by "_", adjacent underscores collapsed; the basename
// also has leading dots stripped so a sanitized hidden file does not look like
// a sentinel) and shortHash is the first 12 hex characters of
// sha256(absFilePath). Determinism and collision resistance are anchored on
// the full absolute path, so two paths with the same basename still produce
// distinct IDs. If the assembled ID would exceed Temporal's 1000-char limit,
// basename is trimmed first so the operator-configured mapping name stays
// visible; only when mapping alone would exceed the budget does mapping get
// trimmed too. The trailing "-{shortHash}" segment is always preserved
// unchanged.
func workflowID(input mediatypes.MediaInput) string {
	sum := sha256.Sum256([]byte(input.FilePath))
	shortHash := hex.EncodeToString(sum[:])[:workflowIDShortHashLen]

	mapping := sanitizeWorkflowIDSegment(input.MappingName, false)
	basename := sanitizeWorkflowIDSegment(filepath.Base(input.FilePath), true)

	// Two literal "-" separators plus the hash are the fixed overhead; the
	// remaining budget is shared between mapping and basename.
	budget := workflowIDMaxLen - (2 + workflowIDShortHashLen)
	if len(mapping)+len(basename) > budget {
		if len(mapping) >= budget {
			mapping = mapping[:budget]
			basename = ""
		} else {
			basename = basename[:budget-len(mapping)]
		}
	}

	return mapping + "-" + basename + "-" + shortHash
}

// workflowIDSegmentInvalid matches any run of characters that should be
// replaced by a single "_" in a sanitized WorkflowID segment: anything outside
// [A-Za-z0-9.-]. Treating "_" as part of the run (rather than allowed) is what
// gives us the "adjacent underscores collapsed" behaviour for free, since runs
// of input underscores are matched and replaced by exactly one "_".
var workflowIDSegmentInvalid = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// sanitizeWorkflowIDSegment restricts s to [A-Za-z0-9._-], replacing any run
// of disallowed characters (or input underscores) with a single "_". When
// stripLeadingDots is true, leading dots are also removed so a sanitized
// hidden filename does not produce an ID that resembles a sentinel file.
func sanitizeWorkflowIDSegment(s string, stripLeadingDots bool) string {
	out := workflowIDSegmentInvalid.ReplaceAllString(s, "_")
	if stripLeadingDots {
		out = strings.TrimLeft(out, ".")
	}

	return out
}

// mediaSearchAttributeKeys are the four typed search attribute keys attached to every
// media workflow. They must be pre-registered in the Temporal namespace before the
// watcher starts with search attributes enabled:
//
//	temporal operator search-attribute create --namespace <namespace> \
//	  --name MediaFilePathSegments --type KeywordList \
//	  --name MediaTitle --type Text \
//	  --name MediaType --type Keyword \
//	  --name MediaMappingName --type Keyword
var (
	mediaFilePathSegmentsKey = temporal.NewSearchAttributeKeyKeywordList("MediaFilePathSegments")
	mediaTitleKey            = temporal.NewSearchAttributeKeyString("MediaTitle")
	mediaTypeKey             = temporal.NewSearchAttributeKeyKeyword("MediaType")
	mediaMappingNameKey      = temporal.NewSearchAttributeKeyKeyword("MediaMappingName")
)

// maxKeywordValueRunes matches the VARCHAR(255) cap that Postgres advanced
// visibility applies to Keyword search attribute columns. Values longer than
// this make the visibility queue processor fail the insert with
// `pq: value too long for type character varying(255)` and silently drop the
// run from the visibility store. KeywordList elements are stored as JSONB and
// not subject to the cap today, but truncating them too keeps the search
// attribute portable across visibility backends.
const maxKeywordValueRunes = 255

// pathSegments splits p on filepath.Separator and truncates each segment.
// Truncation is safe because the full untruncated path is always attached to
// the workflow Memo by buildWorkflowMemo.
func pathSegments(p string) []string {
	parts := strings.Split(p, string(filepath.Separator))
	out := make([]string, 0, len(parts))

	for _, part := range parts {
		if part == "" {
			continue
		}

		out = append(out, truncateToRunes(part, maxKeywordValueRunes))
	}

	return out
}

// truncateToRunes clips s on a rune boundary so the result remains valid UTF-8
// when s contains multi-byte characters. Plain s[:maxRunes] would split them.
func truncateToRunes(s string, maxRunes int) string {
	if len(s) <= maxRunes {
		return s
	}

	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}

	return string(runes[:maxRunes])
}

// buildWorkflowMemo returns the Memo map for a media workflow dispatch. Memo is
// always attached to every run, independent of whether search attributes are enabled.
func buildWorkflowMemo(input mediatypes.MediaInput) map[string]interface{} {
	return map[string]interface{}{
		"MediaFilePath":    input.FilePath,
		"MediaTitle":       filepath.Base(input.FilePath),
		"MediaType":        string(input.MediaType),
		"MediaMappingName": input.MappingName,
		"MediaWatchRoot":   input.WatchRoot,
	}
}

// buildSearchAttributes returns the TypedSearchAttributes for a media workflow dispatch.
// MediaFilePathSegments is computed relative to the watch root because the
// absolute prefix is shared by every workflow from that watch and adds no
// distinguishing information; the watch is identified by MediaMappingName.
func buildSearchAttributes(input mediatypes.MediaInput) temporal.SearchAttributes {
	relPath, _ := filepath.Rel(input.WatchRoot, input.FilePath)

	return temporal.NewSearchAttributes(
		mediaFilePathSegmentsKey.ValueSet(pathSegments(relPath)),
		mediaTitleKey.ValueSet(filepath.Base(input.FilePath)),
		mediaTypeKey.ValueSet(string(input.MediaType)),
		mediaMappingNameKey.ValueSet(truncateToRunes(input.MappingName, maxKeywordValueRunes)),
	)
}

// isMissingSearchAttributeError reports whether err is a Temporal InvalidArgument
// error indicating that one or more custom search attributes are not registered in
// the namespace.
func isMissingSearchAttributeError(err error) bool {
	var invalidArg *serviceerror.InvalidArgument
	if !errors.As(err, &invalidArg) {
		return false
	}

	return strings.Contains(strings.ToLower(invalidArg.Message), "search attribute")
}

// newTemporalDispatch returns a dispatchFunc that calls ExecuteWorkflow on the given
// Temporal client with a deterministic WorkflowID, AllowDuplicate reuse policy, and
// Fail conflict policy.
//
// The reuse policy controls behaviour when the previous run for this WorkflowID has
// already closed; AllowDuplicate is the right fit for the watcher's two re-run
// scenarios:
//   - A previously failed workflow can be retried on the next tick.
//   - A previously completed workflow can run again when an operator removes both the
//     source file and its sentinel and re-adds the file; AllowDuplicate lets Temporal
//     start a fresh run under the same WorkflowID.
//
// The conflict policy controls behaviour when a run for this WorkflowID is currently
// in progress. Fail makes Temporal reject the duplicate, and
// WorkflowExecutionErrorWhenAlreadyStarted opts the Go SDK out of its default
// "swallow the error and attach to the existing run" behaviour so the rejection
// propagates as serviceerror.WorkflowExecutionAlreadyStarted. Without both, every
// duplicate dispatch from a peer watcher would silently be counted as a fresh
// dispatch, over-counting dispatchesTotal under multi-watcher conditions. When the
// conflict fires, the dispatch returns errWorkflowAlreadyStarted so the scan loop
// can suppress both the dispatch and dispatch-error counters for that file.
func newTemporalDispatch(c client.Client, taskQueue string, searchAttributesEnabled bool) dispatchFunc {
	return func(ctx context.Context, input mediatypes.MediaInput) error {
		options := client.StartWorkflowOptions{
			ID:                                       workflowID(input),
			TaskQueue:                                taskQueue,
			WorkflowIDReusePolicy:                    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			WorkflowIDConflictPolicy:                 enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
			WorkflowExecutionErrorWhenAlreadyStarted: true,
			Memo:                                     buildWorkflowMemo(input),
		}

		if searchAttributesEnabled {
			options.TypedSearchAttributes = buildSearchAttributes(input)
		}

		if _, err := c.ExecuteWorkflow(ctx, options, mediatypes.MediaWorkflowName, input); err != nil {
			var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
			if errors.As(err, &alreadyStarted) {
				return errWorkflowAlreadyStarted
			}

			if isMissingSearchAttributeError(err) {
				slog.ErrorContext(ctx, "dispatch failed: custom Temporal search attributes are not registered in the namespace; register them or set WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED=false to disable",
					slog.String("required_attributes", "MediaFilePathSegments (KeywordList), MediaTitle (Text), MediaType (Keyword), MediaMappingName (Keyword)"),
					slog.String("registration_command", "temporal operator search-attribute create --namespace <namespace> --name MediaFilePathSegments --type KeywordList --name MediaTitle --type Text --name MediaType --type Keyword --name MediaMappingName --type Keyword"),
					slog.Any("err", err),
				)
			}

			return err
		}

		return nil
	}
}

// runScanLoop walks every configured watch directory once on entry, then again on
// every tick of cfg.ScanInterval, until ctx is cancelled. Per-tick errors are
// logged and do not abort the loop.
func runScanLoop(ctx context.Context, cfg *Config, instruments *scanInstruments, dispatch dispatchFunc) {
	runOnce := func() {
		if err := scan(ctx, cfg, instruments, dispatch); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}

			slog.ErrorContext(ctx, "scan tick reported errors", slog.Any("err", err))
		}
	}

	runOnce()

	ticker := time.NewTicker(cfg.ScanInterval.Duration())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
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

			instruments.filesDiscoveredTotal.WithLabelValues(w.Name, string(w.MediaType)).Inc()

			filesDiscovered++

			dispatchErr := dispatch(ctx, mediatypes.MediaInput{
				FilePath:               path,
				MediaType:              w.MediaType,
				MappingName:            w.Name,
				PreserveSource:         w.PreserveSource,
				WatchRoot:              absWatchRoot,
				RetainEmptyDirectories: w.RetainEmptyDirectories,
				OutputPath:             absOutputPath,
				OutputRemotePath:       outputRemotePath,
			})

			switch {
			case dispatchErr == nil:
				instruments.dispatchesTotal.WithLabelValues(w.Name, string(w.MediaType)).Inc()

				jobsSubmitted++

				slog.InfoContext(ctx, "dispatched workflow", slog.String("file", path), slog.String("watch", w.Name))
			case errors.Is(dispatchErr, errWorkflowAlreadyStarted):
				slog.DebugContext(ctx, "workflow already running for file (multi-watcher dedup)",
					slog.String("file", path), slog.String("watch", w.Name))
			default:
				mappingErrs = append(mappingErrs, fmt.Errorf("dispatch workflow for %q (media type %v): %w", path, w.MediaType, dispatchErr))

				instruments.dispatchErrorsTotal.WithLabelValues(w.Name, string(w.MediaType)).Inc()
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
		instruments.scanDuration.WithLabelValues(w.Name).Observe(duration)

		if len(mappingErrs) == 0 {
			instruments.scansTotal.WithLabelValues(w.Name, scanStatusSuccess).Inc()
			instruments.lastSuccessfulScan.WithLabelValues(w.Name).Set(float64(time.Now().Unix()))
		} else {
			instruments.scansTotal.WithLabelValues(w.Name, scanStatusError).Inc()

			errs = append(errs, mappingErrs...)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("directory scan completed with errors: %w", err)
	}

	return nil
}
