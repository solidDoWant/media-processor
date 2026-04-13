package ffmpeg

import (
	"context"
	"log/slog"
	"strings"

	"github.com/asticode/go-astiav"
)

func init() {
	// Pass libav log messages through to slog; slog handles level filtering.
	astiav.SetLogLevel(astiav.LogLevelDebug)
	astiav.SetLogCallback(func(_ astiav.Classer, l astiav.LogLevel, _, msg string) {
		ctx := context.Background()
		logger := slog.Default()

		level := astiavLevelToSlog(l)
		if !logger.Enabled(ctx, level) {
			return
		}

		logger.Log(ctx, level, strings.TrimRight(msg, "\n"), "source", "ffmpeg")
	})
}

// astiavLevelToSlog maps astiav log levels to the nearest slog equivalent.
func astiavLevelToSlog(l astiav.LogLevel) slog.Level {
	switch {
	case l <= astiav.LogLevelError:
		return slog.LevelError
	case l <= astiav.LogLevelWarning:
		return slog.LevelWarn
	default:
		// Info level libav logs can still be quite noisy, so treat them as debug output.
		return slog.LevelDebug
	}
}
