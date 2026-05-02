package media

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// WriteSentinel creates a zero-byte hidden sentinel file alongside filePath to
// mark it as processed, preventing the watcher from re-dispatching it.
// The sentinel path is .BASENAME.done in the same directory (e.g. /dl/movie.mkv
// → /dl/.movie.mkv.done). The write is idempotent.
func WriteSentinel(filePath string) error {
	sentinelPath := filepath.Join(filepath.Dir(filePath), "."+filepath.Base(filePath)+".done")
	if err := os.WriteFile(sentinelPath, nil, 0o644); err != nil {
		return fmt.Errorf("write sentinel: %w", err)
	}

	return nil
}

// RunCleanup deletes the original source file after successful processing and,
// unless retainEmptyDirs is true, removes any parent directories that become
// empty as a result, stopping at watchRoot.
func RunCleanup(filePath, watchRoot string, retainEmptyDirs bool) error {
	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete source file: %w", err)
	}

	if !retainEmptyDirs {
		pruneEmptyParents(filePath, watchRoot)
	}

	return nil
}

// pruneEmptyParents removes parent directories of filePath bottom-up as long as each
// directory is empty, stopping when it reaches watchRoot or a non-empty directory.
// The watch root itself is never removed. If watchRoot is empty, no pruning is performed.
// Removal errors are logged and do not propagate; they are expected in concurrent-write
// scenarios (e.g. a new file was written to the directory between the ReadDir check and Remove).
func pruneEmptyParents(filePath, watchRoot string) {
	if watchRoot == "" {
		return
	}

	root, err := os.OpenRoot(watchRoot)
	if err != nil {
		slog.Warn("prune empty parent: failed to open watch root", "watchRoot", watchRoot, "error", err)
		return
	}

	defer func() { _ = root.Close() }()

	dir := filepath.Dir(filePath)

	for {
		relDir, err := filepath.Rel(watchRoot, dir)
		if err != nil {
			slog.Warn("prune empty parent: could not compute relative path", "dir", dir, "watchRoot", watchRoot, "error", err)
			return
		}

		// Reached the watch root — stop without removing it.
		if relDir == "." {
			return
		}

		// relDir escapes watchRoot (e.g. filePath was outside the watch tree). This
		// indicates a misconfiguration or security issue; log and stop.
		if !filepath.IsLocal(relDir) {
			slog.Warn("prune empty parent: directory is outside watch root", "dir", dir, "watchRoot", watchRoot)
			return
		}

		d, err := root.Open(relDir)
		if err != nil {
			return
		}
		// ReadDir(1) reads at most one entry — enough to determine whether the
		// directory is empty without loading the full listing.
		entries, err := d.ReadDir(1)
		_ = d.Close()

		if len(entries) > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return
		}

		if removeErr := root.Remove(relDir); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			slog.Warn("prune empty parent: failed to remove directory", "dir", filepath.Join(watchRoot, relDir), "error", removeErr)
			return
		}

		dir = filepath.Dir(dir)
	}
}
