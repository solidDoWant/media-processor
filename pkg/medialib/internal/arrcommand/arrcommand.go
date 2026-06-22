// Package arrcommand provides shared polling logic for waiting on
// Sonarr/Radarr long-running commands (e.g. DownloadedEpisodesScan,
// DownloadedMoviesScan) to reach a terminal state.
//
// Both services queue a command and return immediately; the caller polls
// /api/v3/command/{id} until the command resolves. The classification of
// terminal states and the unsuccessful-result detection are identical
// between the two, so the loop lives here and each client supplies a thin
// fetch closure that knows how to talk to its starr instance.
package arrcommand

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoSuccessfulImports is returned (wrapped) when a command completes with
// result="unsuccessful". This commonly means the file at the scan path is no
// longer eligible for import — for example, the arr service's own
// completed-download handler raced our explicit scan and already imported
// the file. Callers can detect via errors.Is and post-check whether the
// import actually happened before treating it as a hard failure.
var ErrNoSuccessfulImports = errors.New("no successful imports")

// notUpgradeMarker is the case-insensitive substring Sonarr/Radarr put in a
// queue item's status messages when they refuse to import a release because the
// file already in the library is as good or better. Both the quality variant
// ("Not an upgrade for existing episode/movie file(s)") and the custom-format
// variant ("Not a Custom Format upgrade for existing episode/movie file(s)")
// share "upgrade for existing", which is specific enough not to collide with
// transient import errors.
const notUpgradeMarker = "upgrade for existing"

// IsNotUpgradeRejection reports whether any of the given queue status messages
// indicates the release was rejected because the existing library file is
// already as good or better (a "not an upgrade" / "not a Custom Format upgrade"
// rejection). Such a rejection is permanent: re-running the import scan always
// hits the same comparison, so callers should treat it as a benign skip rather
// than a retryable failure.
func IsNotUpgradeRejection(messages ...string) bool {
	for _, message := range messages {
		if strings.Contains(strings.ToLower(message), notUpgradeMarker) {
			return true
		}
	}

	return false
}

// DefaultPollInterval is used when the caller passes a non-positive interval.
// Sonarr/Radarr command status updates are inexpensive (a single DB read on
// the server side), but importing a season pack can serialize many commands
// in their 3-thread executor — polling too aggressively just adds noise.
const DefaultPollInterval = 2 * time.Second

// Status is the subset of fields Wait inspects from a command response.
// Only these three are needed: status drives the terminal-state classification,
// message is included in error wrapping, and result distinguishes
// command-completed-but-import-failed from command-completed-and-import-applied.
type Status struct {
	// Status is the command lifecycle state (queued, started, completed,
	// failed, aborted, cancelled, orphaned).
	Status string
	// Message is the human-readable note attached to the latest state change.
	Message string
	// Result is the command outcome (unknown, successful, unsuccessful).
	// Sonarr/Radarr set "unsuccessful" when the command itself completed but
	// no useful work was performed (e.g. an import where every file was
	// rejected). Empty/Unknown is treated the same as successful.
	Result string
}

// Fetcher returns the latest status of the command identified by id.
// Returning an error aborts the wait with that error wrapped.
type Fetcher func(ctx context.Context, id int64) (Status, error)

// Wait polls fetch every interval until the command reaches a terminal state.
// Returns nil when the command completed with an empty or successful Result;
// returns an error when:
//   - the command status reports a terminal failure (failed, aborted, cancelled, orphaned),
//   - the command completed with Result="unsuccessful",
//   - fetch returns an error,
//   - or ctx is cancelled.
//
// service ("sonarr"/"radarr") is interpolated into error messages so callers
// can attribute failures without re-wrapping. id == 0 short-circuits to nil
// (no command was issued); a non-positive interval falls back to
// DefaultPollInterval.
func Wait(ctx context.Context, fetch Fetcher, id int64, interval time.Duration, service string) error {
	if id == 0 {
		return nil
	}

	if interval <= 0 {
		interval = DefaultPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		status, err := fetch(ctx, id)
		if err != nil {
			return fmt.Errorf("get %s command status (id=%d): %w", service, id, err)
		}

		switch strings.ToLower(status.Status) {
		case "completed":
			if strings.EqualFold(status.Result, "unsuccessful") {
				return fmt.Errorf("%s command %d completed but reported %w: %s", service, id, ErrNoSuccessfulImports, status.Message)
			}

			return nil
		case "failed", "aborted", "cancelled", "orphaned":
			return fmt.Errorf("%s command %d ended with status %q: %s", service, id, status.Status, status.Message)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
