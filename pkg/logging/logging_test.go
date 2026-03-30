package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/logging"
)

func TestSetup(t *testing.T) {
	tests := []struct {
		input         string
		enabledLevel  slog.Level
		disabledLevel slog.Level
	}{
		{"debug", slog.LevelDebug, slog.Level(slog.LevelDebug - 1)},
		{"DEBUG", slog.LevelDebug, slog.Level(slog.LevelDebug - 1)},
		{"info", slog.LevelInfo, slog.LevelDebug},
		{"INFO", slog.LevelInfo, slog.LevelDebug},
		{"", slog.LevelInfo, slog.LevelDebug},
		{"warn", slog.LevelWarn, slog.LevelInfo},
		{"warning", slog.LevelWarn, slog.LevelInfo},
		{"error", slog.LevelError, slog.LevelWarn},
	}

	for _, tc := range tests {
		t.Run("level="+tc.input, func(t *testing.T) {
			prev := slog.Default()

			t.Cleanup(func() { slog.SetDefault(prev) })

			logging.Setup(tc.input)

			logger := slog.Default()
			assert.True(t, logger.Enabled(t.Context(), tc.enabledLevel),
				"expected level %v to be enabled", tc.enabledLevel)
			assert.False(t, logger.Enabled(t.Context(), tc.disabledLevel),
				"expected level %v to be disabled", tc.disabledLevel)
		})
	}
}

func TestSetupUnrecognised(t *testing.T) {
	prev := slog.Default()

	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	logging.Setup("bogus")

	// Falls back to INFO level.
	logger := slog.Default()
	assert.True(t, logger.Enabled(t.Context(), slog.LevelInfo))
	assert.False(t, logger.Enabled(t.Context(), slog.LevelDebug))

	// Emits a warning containing the bad value.
	require.Contains(t, buf.String(), "bogus", "expected warning to contain the unrecognised value")
	require.True(t, strings.Contains(buf.String(), "WARN") || strings.Contains(buf.String(), "warn"),
		"expected a WARN-level log line")
}
