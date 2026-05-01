package temporalclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/contrib/envconfig"
)

func TestExtractAPIKeyFile(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    string
		expectedKey string
		errFunc     require.ErrorAssertionFunc
	}{
		{
			name:        "empty api key passes through",
			input:       "",
			expected:    "",
			expectedKey: "",
		},
		{
			name:        "inline literal passes through",
			input:       "tmprl_abcdef",
			expected:    "",
			expectedKey: "tmprl_abcdef",
		},
		{
			name:        "file:// with absolute path is extracted and inline value cleared",
			input:       "file:///etc/temporal/api-key",
			expected:    "/etc/temporal/api-key",
			expectedKey: "",
		},
		{
			name:    "file:// with relative path is rejected",
			input:   "file://relative/path",
			errFunc: require.Error,
		},
		{
			name:    "file:// with empty path is rejected",
			input:   "file://",
			errFunc: require.Error,
		},
		{
			name:    "single-slash file: form is rejected",
			input:   "file:/etc/key",
			errFunc: require.Error,
		},
		{
			name:    "bare file: scheme without slashes is rejected",
			input:   "file:",
			errFunc: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errFunc == nil {
				test.errFunc = require.NoError
			}

			profile := envconfig.ClientConfigProfile{APIKey: test.input}

			got, err := extractAPIKeyFile(&profile)
			test.errFunc(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, test.expected, got)
			assert.Equal(t, test.expectedKey, profile.APIKey)
		})
	}
}
