package workflows

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// runMove copies the transcoded file from the system temp directory to outputDir,
// then atomically renames it to the final path. On success it removes the temp
// directory. On rename failure it attempts to remove the intermediate copy.
func runMove(input MovieInput, tc transcodeOutput, outputDir string) error {
	finalPath := filepath.Join(outputDir, filepath.Base(input.FilePath))
	tmpFinalPath := filepath.Join(outputDir, "._"+filepath.Base(input.FilePath)+".tmp")

	if err := copyFile(tc.TempPath, tmpFinalPath); err != nil {
		return fmt.Errorf("copy to output dir: %w", err)
	}

	if err := os.Rename(tmpFinalPath, finalPath); err != nil {
		if removeErr := os.Remove(tmpFinalPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("move output file: %w", err),
				fmt.Errorf("cleanup tmp file: %w", removeErr),
			)
		}
		return fmt.Errorf("move output file: %w", err)
	}

	// Clean up the system temp dir now that the file has been successfully moved.
	// os.RemoveAll returns nil when the path doesn't exist, so no ErrNotExist check needed.
	if err := os.RemoveAll(filepath.Dir(tc.TempPath)); err != nil {
		return fmt.Errorf("remove temp dir: %w", err)
	}

	return nil
}

// copyFile copies the contents of src to dst, creating or truncating dst.
// It propagates both copy and close errors to avoid silently losing buffered data.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()

	if copyErr != nil {
		return fmt.Errorf("copy data: %w", copyErr)
	}

	if closeErr != nil {
		return fmt.Errorf("close destination: %w", closeErr)
	}

	return nil
}
