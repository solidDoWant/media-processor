package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/workflows/media"
)

// TestLoadConfigDefaultsTaskQueueAndActivities verifies that loadConfig fills
// TaskQueuePrefix from the documented default when TEMPORAL_TASK_QUEUE is
// unset, and that an unset WORKER_ACTIVITIES resolves to every known token.
func TestLoadConfigDefaultsTaskQueueAndActivities(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE", "")
	t.Setenv("WORKER_ACTIVITIES", "")
	t.Setenv("RADARR_URL", "http://radarr.local")
	t.Setenv("RADARR_API_KEY", "k")
	t.Setenv("SONARR_URL", "http://sonarr.local")
	t.Setenv("SONARR_API_KEY", "k")

	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, media.DefaultTaskQueuePrefix, cfg.TaskQueuePrefix)
	assert.Equal(t, media.DefaultTaskQueuePrefix, cfg.Workflow.TaskQueuePrefix)

	expectedTokens := append([]string{media.WorkflowToken}, media.KnownActivities...)
	assert.Equal(t, expectedTokens, cfg.EnabledTokens)
}

// TestLoadConfigWorkerActivitiesSelection verifies that loadConfig parses the
// WORKER_ACTIVITIES env var, resolves it against the known token set, and
// surfaces a descriptive error when the resolution fails.
func TestLoadConfigWorkerActivitiesSelection(t *testing.T) {
	t.Setenv("RADARR_URL", "http://radarr.local")
	t.Setenv("RADARR_API_KEY", "k")
	t.Setenv("SONARR_URL", "http://sonarr.local")
	t.Setenv("SONARR_API_KEY", "k")

	t.Run("transcode only", func(t *testing.T) {
		t.Setenv("WORKER_ACTIVITIES", "transcode")

		cfg, err := loadConfig()
		require.NoError(t, err)
		assert.Equal(t, []string{media.TranscodeActivityToken}, cfg.EnabledTokens)
	})

	t.Run("all minus transcode", func(t *testing.T) {
		t.Setenv("WORKER_ACTIVITIES", "all,!transcode")

		cfg, err := loadConfig()
		require.NoError(t, err)
		assert.NotContains(t, cfg.EnabledTokens, media.TranscodeActivityToken)
		assert.Contains(t, cfg.EnabledTokens, media.WorkflowToken)
	})

	t.Run("unknown token errors", func(t *testing.T) {
		t.Setenv("WORKER_ACTIVITIES", "frobnicate")

		_, err := loadConfig()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "frobnicate")
	})
}

func TestParseH265CRF(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected int
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "unset returns 0",
			envValue: "",
			expected: 0,
		},
		{
			name:     "minimum valid value",
			envValue: "1",
			expected: 1,
		},
		{
			name:     "maximum valid value",
			envValue: "51",
			expected: 51,
		},
		{
			name:     "typical quality value",
			envValue: "24",
			expected: 24,
		},
		{
			name:     "zero is out of range",
			envValue: "0",
			errFunc:  require.Error,
		},
		{
			name:     "52 is out of range",
			envValue: "52",
			errFunc:  require.Error,
		},
		{
			name:     "negative value is out of range",
			envValue: "-1",
			errFunc:  require.Error,
		},
		{
			name:     "non-integer returns error naming var and value",
			envValue: "high",
			errFunc:  require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const envVar = "TEST_PARSE_H265_CRF_VAR"
			t.Setenv(envVar, test.envValue)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := parseH265CRF(envVar)
			errFunc(t, err)

			if err != nil {
				if test.envValue != "" {
					assert.Contains(t, err.Error(), envVar)
					assert.Contains(t, err.Error(), test.envValue)
				}

				return
			}

			assert.Equal(t, test.expected, got)
		})
	}
}

// TestWorkerStopTimeoutFallsBackToTranscodeTimeout locks in the requirement
// that an unset WORKER_STOP_TIMEOUT falls back to the effective transcode
// timeout (which itself resolves to media.DefaultTranscodeTimeout when
// MEDIA_TRANSCODE_TIMEOUT is unset). This keeps the drain ceiling coupled to
// the longest expected activity even when the operator raises the transcode
// timeout, so a long-running transcode is not cancelled mid-flight on SIGTERM.
func TestWorkerStopTimeoutFallsBackToTranscodeTimeout(t *testing.T) {
	t.Setenv("WORKER_STOP_TIMEOUT", "")

	got, err := parseTimeout("WORKER_STOP_TIMEOUT", media.DefaultTranscodeTimeout)
	require.NoError(t, err)
	assert.Equal(t, media.DefaultTranscodeTimeout, got)

	overriddenTranscodeTimeout := 8 * time.Hour
	got, err = parseTimeout("WORKER_STOP_TIMEOUT", overriddenTranscodeTimeout)
	require.NoError(t, err)
	assert.Equal(t, overriddenTranscodeTimeout, got)
}

func TestValidateHardwareDevicePath(t *testing.T) {
	tests := []struct {
		name          string
		setupPath     func(t *testing.T) string
		errSubstrings []string
		errFunc       require.ErrorAssertionFunc
	}{
		{
			name:      "empty path skips check",
			setupPath: func(*testing.T) string { return "" },
		},
		{
			name: "missing path errors and names the path",
			setupPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "renderD128")
			},
			errSubstrings: []string{"MEDIA_HARDWARE_DEVICE_PATH", "renderD128"},
			errFunc:       require.Error,
		},
		{
			name: "regular file errors with not a character device",
			setupPath: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "regular")
				f, err := os.Create(p)
				require.NoError(t, err)
				require.NoError(t, f.Close())

				return p
			},
			errSubstrings: []string{"MEDIA_HARDWARE_DEVICE_PATH", "not a character device"},
			errFunc:       require.Error,
		},
		{
			name: "valid character device is accepted",
			setupPath: func(t *testing.T) string {
				const devNull = "/dev/null"

				info, err := os.Stat(devNull)
				if err != nil || info.Mode()&os.ModeCharDevice == 0 {
					t.Skipf("requires /dev/null as a character device on the test host (stat err=%v); rerun on a Unix host where /dev/null is a char device", err)
				}

				return devNull
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			err := validateHardwareDevicePath(test.setupPath(t))
			errFunc(t, err)

			if err != nil {
				for _, errSubstring := range test.errSubstrings {
					assert.Contains(t, err.Error(), errSubstring)
				}
			}
		})
	}
}

func TestParsePositiveInt(t *testing.T) {
	tests := []struct {
		name       string
		envValue   string
		defaultVal int
		expected   int
		errFunc    require.ErrorAssertionFunc
	}{
		{name: "unset returns default", envValue: "", defaultVal: 5, expected: 5},
		{name: "valid positive integer", envValue: "12", defaultVal: 5, expected: 12},
		{name: "zero rejected", envValue: "0", defaultVal: 5, errFunc: require.Error},
		{name: "negative rejected", envValue: "-3", defaultVal: 5, errFunc: require.Error},
		{name: "non-integer rejected", envValue: "five", defaultVal: 5, errFunc: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const envVar = "TEST_PARSE_POSITIVE_INT_VAR"
			t.Setenv(envVar, test.envValue)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := parsePositiveInt(envVar, test.defaultVal)
			errFunc(t, err)

			if err != nil {
				assert.Contains(t, err.Error(), envVar)
				assert.Contains(t, err.Error(), test.envValue)

				return
			}

			assert.Equal(t, test.expected, got)
		})
	}
}

func TestParseUnitFloat(t *testing.T) {
	tests := []struct {
		name       string
		envValue   string
		defaultVal float64
		expected   float64
		errFunc    require.ErrorAssertionFunc
	}{
		{name: "unset returns default", envValue: "", defaultVal: 0.8, expected: 0.8},
		{name: "valid mid-range", envValue: "0.6", defaultVal: 0.8, expected: 0.6},
		{name: "upper bound accepted", envValue: "1", defaultVal: 0.8, expected: 1},
		{name: "below lower bound rejected", envValue: "0", defaultVal: 0.8, errFunc: require.Error},
		{name: "above upper bound rejected", envValue: "1.5", defaultVal: 0.8, errFunc: require.Error},
		{name: "negative rejected", envValue: "-0.1", defaultVal: 0.8, errFunc: require.Error},
		{name: "non-numeric rejected", envValue: "high", defaultVal: 0.8, errFunc: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const envVar = "TEST_PARSE_UNIT_FLOAT_VAR"
			t.Setenv(envVar, test.envValue)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := parseUnitFloat(envVar, test.defaultVal)
			errFunc(t, err)

			if err != nil {
				assert.Contains(t, err.Error(), envVar)
				assert.Contains(t, err.Error(), test.envValue)

				return
			}

			assert.InDelta(t, test.expected, got, 0.0001)
		})
	}
}

// TestLoadTranscodeLimiterConfigDefaults verifies that loadTranscodeLimiterConfig
// returns the documented AC-35 defaults when no MEDIA_TRANSCODE_LIMITER_*
// variables are set.
func TestLoadTranscodeLimiterConfigDefaults(t *testing.T) {
	for _, key := range []string{
		"MEDIA_TRANSCODE_LIMITER_STATIC_CAP",
		"MEDIA_TRANSCODE_LIMITER_GPU_THRESHOLD",
		"MEDIA_TRANSCODE_LIMITER_POST_ADMISSION_COOLDOWN",
		"MEDIA_TRANSCODE_LIMITER_SAMPLE_INTERVAL",
		"MEDIA_TRANSCODE_LIMITER_SMOOTHING_WINDOW",
	} {
		t.Setenv(key, "")
	}

	cfg, err := loadTranscodeLimiterConfig()
	require.NoError(t, err)
	assert.Equal(t, 5, cfg.Limiter.StaticCap)
	assert.InDelta(t, 0.8, cfg.Limiter.GPUThreshold, 0.0001)
	assert.Equal(t, 3*time.Second, cfg.Limiter.PostAdmissionCooldown)
	assert.Equal(t, 500*time.Millisecond, cfg.SampleInterval)
	assert.Equal(t, 5, cfg.SmoothingWindow)
}

// TestLoadTranscodeLimiterConfigOverrides verifies that operator overrides
// flow through to the resolved struct.
func TestLoadTranscodeLimiterConfigOverrides(t *testing.T) {
	t.Setenv("MEDIA_TRANSCODE_LIMITER_STATIC_CAP", "12")
	t.Setenv("MEDIA_TRANSCODE_LIMITER_GPU_THRESHOLD", "0.65")
	t.Setenv("MEDIA_TRANSCODE_LIMITER_POST_ADMISSION_COOLDOWN", "5s")
	t.Setenv("MEDIA_TRANSCODE_LIMITER_SAMPLE_INTERVAL", "250ms")
	t.Setenv("MEDIA_TRANSCODE_LIMITER_SMOOTHING_WINDOW", "10")

	cfg, err := loadTranscodeLimiterConfig()
	require.NoError(t, err)
	assert.Equal(t, 12, cfg.Limiter.StaticCap)
	assert.InDelta(t, 0.65, cfg.Limiter.GPUThreshold, 0.0001)
	assert.Equal(t, 5*time.Second, cfg.Limiter.PostAdmissionCooldown)
	assert.Equal(t, 250*time.Millisecond, cfg.SampleInterval)
	assert.Equal(t, 10, cfg.SmoothingWindow)
}

func TestParseTimeout(t *testing.T) {
	tests := []struct {
		name       string
		envValue   string
		defaultVal time.Duration
		expected   time.Duration
		errFunc    require.ErrorAssertionFunc
	}{
		{
			name:       "unset returns default",
			envValue:   "",
			defaultVal: 30 * time.Minute,
			expected:   30 * time.Minute,
		},
		{
			name:       "unset returns 4h default",
			envValue:   "",
			defaultVal: 4 * time.Hour,
			expected:   4 * time.Hour,
		},
		{
			name:       "valid duration string parsed correctly",
			envValue:   "1h",
			defaultVal: 30 * time.Minute,
			expected:   time.Hour,
		},
		{
			name:       "valid compound duration parsed correctly",
			envValue:   "1h30m",
			defaultVal: 30 * time.Minute,
			expected:   90 * time.Minute,
		},
		{
			name:       "invalid value returns error naming var and value",
			envValue:   "abc",
			defaultVal: 30 * time.Minute,
			errFunc:    require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const envVar = "TEST_PARSE_TIMEOUT_VAR"
			t.Setenv(envVar, test.envValue)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := parseTimeout(envVar, test.defaultVal)
			errFunc(t, err)

			if err != nil {
				assert.Contains(t, err.Error(), envVar)
				assert.Contains(t, err.Error(), test.envValue)

				return
			}

			assert.Equal(t, test.expected, got)
		})
	}
}
