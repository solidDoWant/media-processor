package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChannelConfigLabel(t *testing.T) {
	tests := []struct {
		name         string
		channelCount int
		hasLFE       bool
		expected     string
	}{
		{
			name:         "stereo (2 channels, no LFE) returns 2.0",
			channelCount: 2,
			hasLFE:       false,
			expected:     "2.0",
		},
		{
			name:         "5.1 layout (6 channels, LFE present) returns 5.1",
			channelCount: 6,
			hasLFE:       true,
			expected:     "5.1",
		},
		{
			name:         "7.1 layout (8 channels, LFE present) returns 7.1",
			channelCount: 8,
			hasLFE:       true,
			expected:     "7.1",
		},
		{
			name:         "2.1 layout (3 channels, LFE present) returns 2.1",
			channelCount: 3,
			hasLFE:       true,
			expected:     "2.1",
		},
		{
			name:         "mono (1 channel, no LFE) returns 1.0",
			channelCount: 1,
			hasLFE:       false,
			expected:     "1.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := channelConfigLabel(tt.channelCount, tt.hasLFE)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestStripChannelConfigLabel(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		expected string
	}{
		{
			name:     "empty title stays empty",
			title:    "",
			expected: "",
		},
		{
			name:     "title with no label is unchanged",
			title:    "Commentary",
			expected: "Commentary",
		},
		{
			name:     "plain label at end is stripped",
			title:    "English 5.1",
			expected: "English",
		},
		{
			name:     "plain label only is stripped to empty",
			title:    "5.1",
			expected: "",
		},
		{
			name:     "bracket-wrapped label at end is stripped",
			title:    "English [5.1]",
			expected: "English",
		},
		{
			name:     "bracket-wrapped label at start is stripped",
			title:    "[5.1] English",
			expected: "English",
		},
		{
			name:     "paren-wrapped label is stripped",
			title:    "English (5.1)",
			expected: "English",
		},
		{
			name:     "brace-wrapped label is stripped",
			title:    "English {5.1}",
			expected: "English",
		},
		{
			name:     "angle-bracket-wrapped label is stripped",
			title:    "English <5.1>",
			expected: "English",
		},
		{
			name:     "double-quoted label is stripped",
			title:    `English "5.1"`,
			expected: "English",
		},
		{
			name:     "single-quoted label is stripped",
			title:    "English '5.1'",
			expected: "English",
		},
		{
			name:     "ch suffix (no space) is stripped",
			title:    "English 5.1ch",
			expected: "English",
		},
		{
			name:     "CH suffix (uppercase, no space) is stripped",
			title:    "English 5.1CH",
			expected: "English",
		},
		{
			name:     "ch suffix with space is stripped",
			title:    "English 5.1 ch",
			expected: "English",
		},
		{
			name:     "CH suffix with space is stripped",
			title:    "English 5.1 CH",
			expected: "English",
		},
		{
			name:     "dash-separated label at end is stripped",
			title:    "English - 5.1",
			expected: "English",
		},
		{
			name:     "dash-separated label at start is stripped",
			title:    "5.1 - English",
			expected: "English",
		},
		{
			name:     "pipe-separated label at end is stripped",
			title:    "English | 5.1",
			expected: "English",
		},
		{
			name:     "pipe-separated label at start is stripped",
			title:    "5.1 | English",
			expected: "English",
		},
		{
			name:     "bracket-wrapped label with ch suffix is stripped",
			title:    "English [5.1ch]",
			expected: "English",
		},
		{
			name:     "paren-wrapped label with CH suffix and space is stripped",
			title:    "English (5.1 CH)",
			expected: "English",
		},
		{
			name:     "multiple words with trailing label",
			title:    "Director Commentary 5.1",
			expected: "Director Commentary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripChannelConfigLabel(tt.title)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildAudioStreamTitle(t *testing.T) {
	tests := []struct {
		name        string
		sourceTitle string
		label       string
		expected    string
	}{
		{
			name:        "empty source title returns label only",
			sourceTitle: "",
			label:       "5.1",
			expected:    "5.1",
		},
		{
			name:        "source title with no label gets label appended",
			sourceTitle: "Commentary",
			label:       "5.1",
			expected:    "Commentary 5.1",
		},
		{
			name:        "source title with bracket-wrapped label gets label replaced",
			sourceTitle: "English [5.1]",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with leading bracket label gets label replaced",
			sourceTitle: "[5.1] English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with dash-separated label gets label replaced",
			sourceTitle: "English - 5.1",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with ch suffix gets label replaced",
			sourceTitle: "English 5.1ch",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "label-only source title returns new label",
			sourceTitle: "5.1",
			label:       "7.1",
			expected:    "7.1",
		},
		{
			name:        "stereo label appended correctly",
			sourceTitle: "Commentary",
			label:       "2.0",
			expected:    "Commentary 2.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildAudioStreamTitle(tt.sourceTitle, tt.label)
			assert.Equal(t, tt.expected, got)
		})
	}
}
