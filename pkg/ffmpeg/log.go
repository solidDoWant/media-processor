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

// x265LogLevel returns an x265 log-level string aligned with the current slog
// level. x265 has its own logging that bypasses FFmpeg's av_log callback and
// writes directly to stderr; this value is passed via x265-params so that the
// encoder's native output respects the application log level.
//
// x265 levels: 0=none, 1=error, 2=warning, 3=info, 4=debug, 5=full.
// Since astiavLevelToSlog already maps libav Info→slog Debug, x265 info output
// is only shown when the application is at debug level.
func x265LogLevel() string {
	logger := slog.Default()
	ctx := context.Background()

	switch {
	case logger.Enabled(ctx, slog.LevelDebug):
		return "3" // info — show all x265 messages
	case logger.Enabled(ctx, slog.LevelWarn):
		return "2" // warning
	default:
		return "1" // error
	}
}
