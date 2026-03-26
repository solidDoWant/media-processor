package shared

import (
	"errors"
	"fmt"
	"os"
)

// RunCleanup deletes the original source file after successful processing.
func RunCleanup(filePath string) error {
	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete source file: %w", err)
	}

	return nil
}
