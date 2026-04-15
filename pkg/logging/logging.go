// Package logging configures the global slog handler from a level string.
package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// Setup configures the global slog logger using the provided level string (e.g.
// the value of the LOG_LEVEL environment variable). An empty string is treated
// as "info". An unrecognised value logs a warning and falls back to INFO.
func Setup(level string) {
	var l slog.Level

	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "info", "":
		l = slog.LevelInfo
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		// Warn using the current default before reconfiguring.
		slog.Warn("unrecognised LOG_LEVEL value, falling back to INFO", "value", level)
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}

// NewZerologLogger returns a zerolog.Logger that routes every entry through the
// default slog handler via zerologSlogBridge. Output is therefore formatted and
// levelled consistently with the rest of the application. The service name is
// attached as a "service" attribute on every entry.
// zerolog is configured at TraceLevel so all entries pass through; slog applies
// the active level filter inside the bridge.
func NewZerologLogger(service string) zerolog.Logger {
	return zerolog.New(&zerologSlogBridge{}).Level(zerolog.TraceLevel).With().Str("service", service).Logger()
}

// zerologSlogBridge is an io.Writer for zerolog that re-emits each log entry
// through the default slog handler. zerolog guarantees that Write is called
// exactly once per complete log entry, so the bridge can parse and forward
// without buffering.
type zerologSlogBridge struct{}

func (b *zerologSlogBridge) Write(p []byte) (int, error) {
	ctx := context.TODO()

	var raw map[string]any
	if err := json.Unmarshal(bytes.TrimRight(p, "\n"), &raw); err != nil {
		slog.Warn("hatchet: non-JSON log entry", "raw", strings.TrimRight(string(p), "\n"))

		return len(p), nil
	}

	lvlStr, _ := raw["level"].(string)
	lvl := zerologLevelToSlog(lvlStr)

	if !slog.Default().Enabled(ctx, lvl) {
		return len(p), nil
	}

	msg, _ := raw["message"].(string)

	attrs := make([]any, 0, len(raw)*2)

	for k, v := range raw {
		if k == "level" || k == "message" || k == "time" {
			continue
		}

		attrs = append(attrs, k, v)
	}

	slog.Log(ctx, lvl, msg, attrs...)

	return len(p), nil
}

// zerologLevelToSlog maps a zerolog level string to the equivalent slog.Level.
func zerologLevelToSlog(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal", "panic":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
