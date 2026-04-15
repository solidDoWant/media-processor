//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// slogWriter is an io.Writer that forwards each line of subprocess output to
// slog at the given level, tagged with the source label (e.g. "docker", "make").
type slogWriter struct {
	level  slog.Level
	source string
	buf    []byte
}

func (w *slogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)

	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}

		line := string(bytes.TrimRight(w.buf[:idx], "\r"))
		if line != "" {
			slog.Log(context.Background(), w.level, line, "source", w.source)
		}

		w.buf = w.buf[idx+1:]
	}

	return len(p), nil
}

// Flush emits any buffered output that did not end with a newline.
// Call this after the subprocess exits to avoid losing the final partial line.
func (w *slogWriter) Flush() {
	line := string(bytes.TrimRight(w.buf, "\r"))
	if line != "" {
		slog.Log(context.Background(), w.level, line, "source", w.source)
	}

	w.buf = nil
}

// newSlogWriter returns a *slogWriter that routes subprocess output to slog.
// Use LevelInfo for stdout and LevelWarn for stderr.
// Call Flush after the subprocess exits to emit any final partial line.
func newSlogWriter(level slog.Level, source string) *slogWriter {
	return &slogWriter{level: level, source: source}
}

// outputFileInfo holds the key media properties of a transcoded output file,
// as reported by ffprobe.
type outputFileInfo struct {
	formatName  string
	videoCodec  string
	durationSec float64
}

// probeOutputFile runs ffprobe on the file at path and returns its media
// properties. The test fails immediately if ffprobe cannot be executed or the
// output cannot be parsed.
func probeOutputFile(t *testing.T, path string) outputFileInfo {
	t.Helper()

	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)

	out, err := cmd.Output()
	require.NoError(t, err, "ffprobe failed on %s", path)

	var result struct {
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}

	require.NoError(t, json.Unmarshal(out, &result), "parse ffprobe output for %s", path)

	info := outputFileInfo{
		formatName: result.Format.FormatName,
	}

	for _, stream := range result.Streams {
		if stream.CodecType == "video" {
			info.videoCodec = stream.CodecName
			break
		}
	}

	if d, parseErr := strconv.ParseFloat(result.Format.Duration, 64); parseErr == nil {
		info.durationSec = d
	}

	return info
}

// findMKV walks dir and returns the path of the first .mkv file found.
// Returns an empty string if no .mkv is present.
func findMKV(t *testing.T, dir string) string {
	t.Helper()

	var found string

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(path), ".mkv") {
			found = path

			return fs.SkipAll
		}

		return nil
	})

	require.NoError(t, err, "walking %s for .mkv", dir)

	return found
}
