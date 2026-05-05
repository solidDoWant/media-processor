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
// enabled set and 0 for the rest. The metric is present on every worker pod
// regardless of which activities it runs.
func TestRegisterActivityEnabledMetric(t *testing.T) {
	tests := []struct {
		name           string
		enabled        []string
		expectedActive map[string]bool
		errFunc        require.ErrorAssertionFunc
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
			errFunc := test.errFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			reg := prometheus.NewRegistry()
			registerActivityEnabledMetric(reg, test.enabled)

			families, err := reg.Gather()
			errFunc(t, err)

			var metricFamily *dto.MetricFamily

			for _, family := range families {
				if family.GetName() == "media_worker_activity_enabled" {
					metricFamily = family
					break
				}
			}

			require.NotNil(t, metricFamily, "media_worker_activity_enabled gauge not registered")

			seenSeries := map[string]float64{}

			for _, metric := range metricFamily.GetMetric() {
				var activity string

				for _, label := range metric.GetLabel() {
					if label.GetName() == "activity" {
						activity = label.GetValue()
						break
					}
				}

				require.NotEmpty(t, activity, "metric series missing activity label")
				seenSeries[activity] = metric.GetGauge().GetValue()
			}

			// One series per known activity.
			assert.Len(t, seenSeries, len(media.KnownActivities))

			for _, activity := range media.KnownActivities {
				expected := 0.0
				if test.expectedActive[activity] {
					expected = 1
				}

				value, ok := seenSeries[activity]
				assert.True(t, ok, "missing series for activity %q", activity)
				assert.Equal(t, expected, value, "activity %q expected value %v, got %v", activity, expected, value)
			}
		})
	}
}
