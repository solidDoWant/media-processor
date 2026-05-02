package media

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

func TestAudioLangName(t *testing.T) {
	tests := []struct {
		name     string
		tag      string
		expected string
	}{
		{
			name:     "empty tag returns Unknown Language",
			tag:      "",
			expected: "Unknown Language",
		},
		{
			name:     "und returns Unknown Language",
			tag:      "und",
			expected: "Unknown Language",
		},
		{
			name:     "eng returns English",
			tag:      "eng",
			expected: "English",
		},
		{
			name:     "unknown tag is returned as-is",
			tag:      "xyz",
			expected: "xyz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := audioLangName(tt.tag)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestDisambiguateLang(t *testing.T) {
	tests := []struct {
		name     string
		streams  []AudioStreamInfo
		expected bool
	}{
		{
			name:     "empty slice returns false",
			streams:  nil,
			expected: false,
		},
		{
			name:     "single stream returns false",
			streams:  []AudioStreamInfo{{StreamInfo: StreamInfo{Language: "eng"}}},
			expected: false,
		},
		{
			name:     "all streams same language returns false",
			streams:  []AudioStreamInfo{{StreamInfo: StreamInfo{Language: "eng"}}, {StreamInfo: StreamInfo{Language: "eng"}}},
			expected: false,
		},
		{
			name:     "all streams und returns false",
			streams:  []AudioStreamInfo{{StreamInfo: StreamInfo{Language: "und"}}, {StreamInfo: StreamInfo{Language: "und"}}},
			expected: false,
		},
		{
			name:     "streams with different languages returns true",
			streams:  []AudioStreamInfo{{StreamInfo: StreamInfo{Language: "eng"}}, {StreamInfo: StreamInfo{Language: "jpn"}}},
			expected: true,
		},
		{
			name:     "eng and und returns true",
			streams:  []AudioStreamInfo{{StreamInfo: StreamInfo{Language: "eng"}}, {StreamInfo: StreamInfo{Language: "und"}}},
			expected: true,
		},
		{
			name:     "empty tag and und treated as same language returns false",
			streams:  []AudioStreamInfo{{StreamInfo: StreamInfo{Language: ""}}, {StreamInfo: StreamInfo{Language: "und"}}},
			expected: false,
		},
		{
			name:     "eng and empty tag returns true",
			streams:  []AudioStreamInfo{{StreamInfo: StreamInfo{Language: "eng"}}, {StreamInfo: StreamInfo{Language: ""}}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := disambiguateLang(tt.streams)
			assert.Equal(t, tt.expected, got)
		})
	}
}
