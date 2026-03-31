package ffmpeg

import (
	"context"
	"log/slog"
	"strings"

	"github.com/asticode/go-astiav"
)

func init() {
	// Pass libav log messages through to slog; slog handles level filtering.
	// LogLevelDebug is intentionally avoided: go-astiav's log.c uses a fixed
	// 1024-byte char buffer with vsprintf, and FFmpeg emits several debug
	// messages longer than 1024 bytes (e.g. the pixel-format negotiation list
	// logged during avfilter_graph_config), which silently overflows that
	// buffer and triggers glibc's stack-smashing protection.
	astiav.SetLogLevel(astiav.LogLevelInfo)
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
	case l <= astiav.LogLevelInfo:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}
