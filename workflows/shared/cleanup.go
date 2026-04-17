package shared

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

	cleanRoot := filepath.Clean(watchRoot)
	dir := filepath.Dir(filePath)

	for {
		cleanDir := filepath.Clean(dir)

		if cleanDir == cleanRoot {
			return
		}

		// Stop if dir is not a descendant of watchRoot — protects against misconfiguration
		// where filePath is outside the watch tree.
		if !strings.HasPrefix(cleanDir+string(filepath.Separator), cleanRoot+string(filepath.Separator)) {
			return
		}

		entries, err := os.ReadDir(cleanDir)
		if err != nil || len(entries) > 0 {
			return
		}

		if removeErr := os.Remove(cleanDir); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			slog.Warn("prune empty parent: failed to remove directory", "dir", cleanDir, "error", removeErr)

			return
		}

		dir = filepath.Dir(dir)
	}
}
