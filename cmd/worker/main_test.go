package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/workflows/media"
)

// TestRegisterActivityEnabledMetric verifies that media_worker_activity_enabled
// emits one series per known activity, with value 1 for tokens in the
// enabled set and 0 for the rest. Verifies AC-33: the metric is present on
// every worker pod regardless of which activities it runs.
func TestRegisterActivityEnabledMetric(t *testing.T) {
	tests := []struct {
		name           string
		enabled        []string
		expectedActive map[string]bool
	}{
		{
			name:    "transcode-only worker",
			enabled: []string{media.TranscodeActivityToken},
			expectedActive: map[string]bool{
				media.TranscodeActivityToken: true,
			},
		},
		{
			name: "all activities except transcode",
			enabled: []string{
				media.WorkflowToken,
				media.ProbeActivityToken,
				media.DetectCropActivityToken,
				media.NotifyActivityToken,
				media.CleanupActivityToken,
				media.NotifyFailureActivityToken,
			},
			expectedActive: map[string]bool{
				media.ProbeActivityToken:         true,
				media.DetectCropActivityToken:    true,
				media.NotifyActivityToken:        true,
				media.CleanupActivityToken:       true,
				media.NotifyFailureActivityToken: true,
			},
		},
		{
			name:           "no activities (workflow-only worker)",
			enabled:        []string{media.WorkflowToken},
			expectedActive: map[string]bool{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			registerActivityEnabledMetric(reg, test.enabled)

			families, err := reg.Gather()
			require.NoError(t, err)

			var family *dto.MetricFamily

			for _, candidate := range families {
				if candidate.GetName() == "media_worker_activity_enabled" {
					family = candidate
					break
				}
			}

			require.NotNil(t, family, "media_worker_activity_enabled gauge not registered")

			seenTokens := map[string]float64{}

			for _, metric := range family.GetMetric() {
				var token string

				for _, label := range metric.GetLabel() {
					if label.GetName() == "activity" {
						token = label.GetValue()
						break
					}
				}

				require.NotEmpty(t, token, "metric series missing activity label")
				seenTokens[token] = metric.GetGauge().GetValue()
			}

			// One series per known activity (AC-33).
			assert.Len(t, seenTokens, len(media.KnownActivities))

			for _, token := range media.KnownActivities {
				expected := 0.0
				if test.expectedActive[token] {
					expected = 1
				}

				value, ok := seenTokens[token]
				assert.True(t, ok, "missing series for activity %q", token)
				assert.InDelta(t, expected, value, 0.0001, "activity %q expected value %v, got %v", token, expected, value)
			}
		})
	}
}
