//go:build e2e

package e2e_test

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// ---- BBB fixture --------------------------------------------------------

func ensureBBBFixture() (string, error) {
	// testdata/cache/ is relative to the e2e package directory.
	// Use an absolute path so that callers (e.g. the qbt stub) can use it as a
	// symlink target that resolves correctly regardless of working directory.
	cacheDir, err := filepath.Abs("testdata/cache")
	if err != nil {
		return "", fmt.Errorf("abs cache dir: %w", err)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache: %w", err)
	}

	mp4Path := filepath.Join(cacheDir, bbbMP4Name)

	if _, err := os.Stat(mp4Path); err == nil {
		log.Printf("e2e: BBB fixture already cached at %s", mp4Path)

		return mp4Path, nil
	}

	log.Printf("e2e: downloading BBB fixture from %s (this may take a while)...", bbbZipURL)

	zipPath := filepath.Join(cacheDir, bbbMP4Name+".zip")

	if err := downloadFile(bbbZipURL, zipPath); err != nil {
		return "", fmt.Errorf("download BBB zip: %w", err)
	}

	log.Printf("e2e: extracting BBB mp4 from zip...")

	if err := extractFromZip(zipPath, bbbMP4Name, mp4Path); err != nil {
		_ = os.Remove(zipPath)

		return "", fmt.Errorf("extract BBB mp4: %w", err)
	}

	// Remove the zip; only the mp4 needs to be cached.
	_ = os.Remove(zipPath)

	log.Printf("e2e: BBB fixture cached at %s", mp4Path)

	return mp4Path, nil
}

func downloadFile(rawURL, dest string) error {
	resp, err := http.Get(rawURL) //nolint:noctx // setup code, no request context needed
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	_, err = io.Copy(f, resp.Body)

	return err
}

// extractFromZip extracts the first file named targetName (basename match) from
// the zip at zipPath into destPath. It writes to a .tmp file first and renames
// on success to ensure the cached file is always complete.
func extractFromZip(zipPath, targetName, destPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}

	defer func() { _ = r.Close() }()

	for _, file := range r.File {
		if filepath.Base(file.Name) != targetName {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %s: %w", file.Name, err)
		}

		tmpPath := destPath + ".tmp"

		out, err := os.Create(tmpPath)
		if err != nil {
			_ = rc.Close()

			return err
		}

		if _, err = io.Copy(out, rc); err != nil {
			_ = rc.Close()
			_ = out.Close()
			_ = os.Remove(tmpPath)

			return fmt.Errorf("copy entry: %w", err)
		}

		_ = rc.Close()

		if err = out.Close(); err != nil {
			_ = os.Remove(tmpPath)

			return err
		}

		return os.Rename(tmpPath, destPath)
	}

	return fmt.Errorf("entry %q not found in zip", targetName)
}
