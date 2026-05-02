package media

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
		{
			name:         "zero channel count with LFE flag clamps non-LFE to 0",
			channelCount: 0,
			hasLFE:       true,
			expected:     "0.1",
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
		{
			name:     "Dolby Atmos 7.1.4 label is not partially stripped",
			title:    "Atmos 7.1.4",
			expected: "Atmos 7.1.4",
		},
		{
			name:     "label with Y digit 2 is not stripped",
			title:    "English 5.2",
			expected: "English 5.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripChannelConfigLabel(tt.title)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestStripLanguageName(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		langName string
		expected string
	}{
		{
			name:     "empty lang name returns title unchanged",
			title:    "English Commentary",
			langName: "",
			expected: "English Commentary",
		},
		{
			name:     "language name at end is stripped",
			title:    "Commentary English",
			langName: "English",
			expected: "Commentary",
		},
		{
			name:     "language name at start is stripped",
			title:    "English Commentary",
			langName: "English",
			expected: "Commentary",
		},
		{
			name:     "language name only is stripped to empty",
			title:    "English",
			langName: "English",
			expected: "",
		},
		{
			name:     "case-insensitive match is stripped",
			title:    "ENGLISH Commentary",
			langName: "English",
			expected: "Commentary",
		},
		{
			name:     "partial word match is not stripped",
			title:    "Englishmen speak",
			langName: "English",
			expected: "Englishmen speak",
		},
		{
			name:     "empty title returns empty",
			title:    "",
			langName: "English",
			expected: "",
		},
		{
			name:     "title with no match is unchanged",
			title:    "Director Commentary",
			langName: "English",
			expected: "Director Commentary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripLanguageName(tt.title, tt.langName)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildAudioStreamTitle(t *testing.T) {
	tests := []struct {
		name        string
		sourceTitle string
		langName    string
		label       string
		expected    string
	}{
		{
			name:        "empty source title with lang and label returns lang and label",
			sourceTitle: "",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "empty source title with no lang returns label only",
			sourceTitle: "",
			langName:    "",
			label:       "5.1",
			expected:    "5.1",
		},
		{
			name:        "content-only source title gets lang and label appended",
			sourceTitle: "Commentary",
			langName:    "English",
			label:       "5.1",
			expected:    "Commentary English 5.1",
		},
		{
			name:        "source title with channel label only gets lang and new label",
			sourceTitle: "5.1",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with lang indicator only gets lang and label",
			sourceTitle: "English",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with lang and channel label gets deduplicated",
			sourceTitle: "English 5.1",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with bracket-wrapped label gets label replaced",
			sourceTitle: "English [5.1]",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with leading bracket label gets label replaced",
			sourceTitle: "[5.1] English",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with dash-separated label gets label replaced",
			sourceTitle: "English - 5.1",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "source title with ch suffix gets label replaced",
			sourceTitle: "English 5.1ch",
			langName:    "English",
			label:       "5.1",
			expected:    "English 5.1",
		},
		{
			name:        "director commentary with lang inserted before label",
			sourceTitle: "Director Commentary",
			langName:    "English",
			label:       "5.1",
			expected:    "Director Commentary English 5.1",
		},
		{
			name:        "no lang — content title gets label appended (channel-config-only path)",
			sourceTitle: "Commentary",
			langName:    "",
			label:       "5.1",
			expected:    "Commentary 5.1",
		},
		{
			name:        "no lang — label-only source title returns new label",
			sourceTitle: "5.1",
			langName:    "",
			label:       "7.1",
			expected:    "7.1",
		},
		{
			name:        "unknown tag used as lang name fallback",
			sourceTitle: "",
			langName:    "zxx",
			label:       "5.1",
			expected:    "zxx 5.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildAudioStreamTitle(tt.sourceTitle, tt.langName, tt.label)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildSubtitleStreamTitle(t *testing.T) {
	tests := []struct {
		name        string
		sourceTitle string
		langName    string
		expected    string
	}{
		{
			name:        "empty lang name returns source title unchanged",
			sourceTitle: "SDH",
			langName:    "",
			expected:    "SDH",
		},
		{
			name:        "empty lang name with empty source title returns empty",
			sourceTitle: "",
			langName:    "",
			expected:    "",
		},
		{
			name:        "content title gets lang name appended",
			sourceTitle: "SDH",
			langName:    "English",
			expected:    "SDH English",
		},
		{
			name:        "empty source title with lang returns lang only",
			sourceTitle: "",
			langName:    "English",
			expected:    "English",
		},
		{
			name:        "lang-only source title is deduplicated",
			sourceTitle: "English",
			langName:    "English",
			expected:    "English",
		},
		{
			name:        "lang indicator stripped and lang appended",
			sourceTitle: "English SDH",
			langName:    "English",
			expected:    "SDH English",
		},
		{
			name:        "unknown tag used as lang name fallback",
			sourceTitle: "",
			langName:    "zxx",
			expected:    "zxx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSubtitleStreamTitle(tt.sourceTitle, tt.langName)
			assert.Equal(t, tt.expected, got)
		})
	}
}
