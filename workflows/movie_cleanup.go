package workflows

import (
	"errors"
	"fmt"
	"os"
)

// runCleanup deletes the original source file after successful processing.
func runCleanup(filePath string) error {
	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete source file: %w", err)
	}

	return nil
}
