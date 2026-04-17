package steps

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIso639Name(t *testing.T) {
	tests := []struct {
		name     string
		tag      string
		expected string
	}{
		{
			name:     "empty tag returns empty string",
			tag:      "",
			expected: "",
		},
		{
			name:     "eng returns English",
			tag:      "eng",
			expected: "English",
		},
		{
			name:     "fra returns French",
			tag:      "fra",
			expected: "French",
		},
		{
			name:     "deu returns German",
			tag:      "deu",
			expected: "German",
		},
		{
			name:     "spa returns Spanish",
			tag:      "spa",
			expected: "Spanish",
		},
		{
			name:     "jpn returns Japanese",
			tag:      "jpn",
			expected: "Japanese",
		},
		{
			name:     "und returns Undetermined",
			tag:      "und",
			expected: "Undetermined",
		},
		{
			name:     "unknown tag is returned as-is",
			tag:      "xyz",
			expected: "xyz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := iso639Name(tt.tag)
			assert.Equal(t, tt.expected, got)
		})
	}
}
