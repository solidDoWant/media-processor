package envvar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireEnv(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected string
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "set returns value",
			envValue: "media-processor",
			expected: "media-processor",
		},
		{
			name:     "empty returns error naming variable",
			envValue: "",
			errFunc:  require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const envVar = "TEST_REQUIRE_ENV_VAR"
			t.Setenv(envVar, test.envValue)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := RequireEnv(envVar)
			errFunc(t, err)

			if err != nil {
				assert.Contains(t, err.Error(), envVar)
				assert.Contains(t, err.Error(), "is not set")

				return
			}

			assert.Equal(t, test.expected, got)
		})
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		name       string
		envValue   string
		defaultVal bool
		expected   bool
		errFunc    require.ErrorAssertionFunc
	}{
		{
			name:       "unset returns default false",
			envValue:   "",
			defaultVal: false,
			expected:   false,
		},
		{
			name:       "unset returns default true",
			envValue:   "",
			defaultVal: true,
			expected:   true,
		},
		{
			name:     "lowercase true",
			envValue: "true",
			expected: true,
		},
		{
			name:     "uppercase TRUE",
			envValue: "TRUE",
			expected: true,
		},
		{
			name:     "mixed-case True",
			envValue: "True",
			expected: true,
		},
		{
			name:     "1 is true",
			envValue: "1",
			expected: true,
		},
		{
			name:     "t is true",
			envValue: "t",
			expected: true,
		},
		{
			name:     "lowercase false",
			envValue: "false",
			expected: false,
		},
		{
			name:     "uppercase FALSE",
			envValue: "FALSE",
			expected: false,
		},
		{
			name:     "0 is false",
			envValue: "0",
			expected: false,
		},
		{
			name:     "garbage returns error naming variable and value",
			envValue: "garbage",
			errFunc:  require.Error,
		},
		{
			name:     "yes is rejected (strconv.ParseBool semantics)",
			envValue: "yes",
			errFunc:  require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const envVar = "TEST_PARSE_BOOL_VAR"
			t.Setenv(envVar, test.envValue)

			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := ParseBool(envVar, test.defaultVal)
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
