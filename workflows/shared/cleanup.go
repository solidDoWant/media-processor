package shared

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

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
	defer root.Close()

	dir := filepath.Dir(filePath)

	for {
		rel, err := filepath.Rel(watchRoot, dir)
		// rel == "." means dir IS the watch root; !filepath.IsLocal catches paths that
		// escape the root (e.g. "..") — both are stopping conditions.
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			return
		}

		d, err := root.Open(rel)
		if err != nil {
			return
		}
		entries, err := d.ReadDir(-1)
		_ = d.Close()
		if err != nil || len(entries) > 0 {
			return
		}

		if removeErr := root.Remove(rel); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			slog.Warn("prune empty parent: failed to remove directory", "dir", filepath.Join(watchRoot, rel), "error", removeErr)
			return
		}

		dir = filepath.Dir(dir)
	}
}
