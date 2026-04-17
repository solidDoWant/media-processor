package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_MissingToken verifies that the worker exits with a descriptive error
// when HATCHET_CLIENT_TOKEN is not set.
func TestRun_MissingToken(t *testing.T) {
	t.Setenv("HATCHET_CLIENT_TOKEN", "")

	err := run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HATCHET_CLIENT_TOKEN")
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
