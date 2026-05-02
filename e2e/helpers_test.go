//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
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

	cmd := exec.CommandContext(t.Context(), "ffprobe",
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

// metricSeries wraps the metric-family map returned by expfmt.TextParser,
// keyed by the base metric name (e.g. "media_workflow_transcode_duration_seconds").
type metricSeries map[string]*dto.MetricFamily

// sum returns the combined value of all samples of the named metric that match
// every (key, value) pair in filter. An empty filter matches all samples.
//
// Names ending in _count or _sum are resolved against the histogram base name:
// "foo_count" → foo.SampleCount, "foo_sum" → foo.SampleSum.
// All other names are read from their Counter or Gauge field.
func (m metricSeries) sum(name string, filter map[string]string) float64 {
	var (
		baseName string
		getValue func(*dto.Metric) float64
	)

	switch {
	case strings.HasSuffix(name, "_count"):
		baseName = strings.TrimSuffix(name, "_count")
		getValue = func(metric *dto.Metric) float64 { return float64(metric.GetHistogram().GetSampleCount()) }
	case strings.HasSuffix(name, "_sum"):
		baseName = strings.TrimSuffix(name, "_sum")
		getValue = func(metric *dto.Metric) float64 { return metric.GetHistogram().GetSampleSum() }
	default:
		baseName = name
		getValue = func(metric *dto.Metric) float64 {
			if c := metric.GetCounter(); c != nil {
				return c.GetValue()
			}

			return metric.GetGauge().GetValue()
		}
	}

	family := m[baseName]
	if family == nil {
		return 0
	}

	var total float64

	for _, metric := range family.GetMetric() {
		if labelsMatch(metric.GetLabel(), filter) {
			total += getValue(metric)
		}
	}

	return total
}

// labelsMatch reports whether labels contains all key/value pairs in filter.
func labelsMatch(labels []*dto.LabelPair, filter map[string]string) bool {
	for key, want := range filter {
		found := false

		for _, lp := range labels {
			if lp.GetName() == key {
				found = lp.GetValue() == want
				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

// fetchMetrics GETs /metrics on addr, parses the Prometheus text exposition,
// and returns the metric families keyed by base name. Network, protocol, and
// parse errors fail the test immediately.
func fetchMetrics(t *testing.T, addr string) metricSeries {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "GET /metrics from %s", addr)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode, "unexpected status from %s", addr)

	parser := expfmt.NewTextParser(model.UTF8Validation)

	families, err := parser.TextToMetricFamilies(resp.Body)
	require.NoError(t, err, "parse /metrics from %s", addr)

	return metricSeries(families)
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
