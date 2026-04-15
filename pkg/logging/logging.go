// Package logging configures the global slog handler from a level string.
package logging

import (
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

// ZerologLevel returns a zerolog-compatible level string for the given
// LOG_LEVEL value. This is useful for configuring third-party libraries (like
// the Hatchet SDK) that use zerolog internally.
func ZerologLevel(level string) string {
	switch strings.ToLower(level) {
	case "debug":
		return "debug"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	default:
		return "info"
	}
}

// NewZerologLogger returns a zerolog.Logger writing JSON to stderr, configured
// at the level derived from the given LOG_LEVEL string and tagged with the
// given service name. Use this to configure Hatchet SDK worker loggers so they
// respect LOG_LEVEL rather than always defaulting to debug.
func NewZerologLogger(level, service string) zerolog.Logger {
	// ZerologLevel always returns a valid zerolog level string, so ParseLevel
	// cannot fail here.
	lvl, _ := zerolog.ParseLevel(ZerologLevel(level))

	return zerolog.New(os.Stderr).Level(lvl).With().Timestamp().Str("service", service).Logger()
}
