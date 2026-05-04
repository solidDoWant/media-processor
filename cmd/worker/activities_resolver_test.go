package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/workflows/media"
)

// workerKnownTokens is the set of WORKER_ACTIVITIES tokens recognised by the
// production worker (the workflow token plus every activity).
func workerKnownTokens() []string {
	known := make([]string, 0, len(media.KnownActivities)+1)
	known = append(known, media.WorkflowToken)
	known = append(known, media.KnownActivities...)

	return known
}

func TestResolveActivities(t *testing.T) {
	known := workerKnownTokens()

	tests := []struct {
		name     string
		tokens   []string
		expected []string
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "all expands to every known token in canonical order",
			tokens:   []string{"all"},
			expected: known,
		},
		{
			name:   "all minus transcode removes transcode but keeps the rest",
			tokens: []string{"all", "!transcode"},
			expected: []string{
				media.WorkflowToken,
				media.ProbeActivityToken,
				media.DetectCropActivityToken,
				media.NotifyActivityToken,
				media.CleanupActivityToken,
				media.NotifyFailureActivityToken,
			},
		},
		{
			name:     "single literal token",
			tokens:   []string{"transcode"},
			expected: []string{media.TranscodeActivityToken},
		},
		{
			name:   "two literal tokens are returned in canonical order",
			tokens: []string{"detect-crop", "probe"},
			expected: []string{
				media.ProbeActivityToken,
				media.DetectCropActivityToken,
			},
		},
		{
			name:     "workflow only",
			tokens:   []string{"workflow"},
			expected: []string{media.WorkflowToken},
		},
		{
			name:    "unknown token errors",
			tokens:  []string{"all", "frobnicate"},
			errFunc: require.Error,
		},
		{
			name:    "unknown negated token errors",
			tokens:  []string{"all", "!frobnicate"},
			errFunc: require.Error,
		},
		{
			name:    "empty result errors",
			tokens:  []string{"all", "!workflow", "!probe", "!detect-crop", "!transcode", "!notify", "!cleanup", "!notify-failure"},
			errFunc: require.Error,
		},
		{
			name:    "no tokens errors",
			tokens:  []string{},
			errFunc: require.Error,
		},
		{
			name:     "literal then negate cancels",
			tokens:   []string{"transcode", "!transcode", "probe"},
			expected: []string{media.ProbeActivityToken},
		},
		{
			name:     "duplicates collapse",
			tokens:   []string{"transcode", "transcode"},
			expected: []string{media.TranscodeActivityToken},
		},
		{
			name:   "whitespace around tokens is tolerated",
			tokens: []string{" all ", " !transcode "},
			expected: []string{
				media.WorkflowToken,
				media.ProbeActivityToken,
				media.DetectCropActivityToken,
				media.NotifyActivityToken,
				media.CleanupActivityToken,
				media.NotifyFailureActivityToken,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := resolveActivities(test.tokens, known)
			errFunc(t, err)

			if err == nil {
				assert.Equal(t, test.expected, got)
			}
		})
	}
}

func TestParseWorkerActivities(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		expected []string
	}{
		{name: "empty defaults to all", raw: "", expected: []string{"all"}},
		{name: "whitespace defaults to all", raw: "   ", expected: []string{"all"}},
		{name: "single token", raw: "transcode", expected: []string{"transcode"}},
		{name: "comma separated", raw: "all,!transcode", expected: []string{"all", "!transcode"}},
		{name: "trims spaces around items", raw: " all , !transcode ", expected: []string{"all", "!transcode"}},
		{name: "drops empty entries", raw: "all,,!transcode,", expected: []string{"all", "!transcode"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, parseWorkerActivities(test.raw))
		})
	}
}
