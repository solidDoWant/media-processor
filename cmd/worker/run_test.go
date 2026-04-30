package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_MissingTaskQueue verifies that the worker exits with a descriptive
// error when TEMPORAL_TASK_QUEUE is not set.
func TestRun_MissingTaskQueue(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE", "")

	err := run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPORAL_TASK_QUEUE")
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
