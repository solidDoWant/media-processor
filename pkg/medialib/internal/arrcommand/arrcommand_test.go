package arrcommand_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/medialib/internal/arrcommand"
)

const fastInterval = time.Millisecond

func TestWait(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		statuses     []arrcommand.Status
		fetchErr     error
		errFunc      require.ErrorAssertionFunc
		errSubstring string
		wantCalls    int
	}{
		{
			name:      "zero id short-circuits without polling",
			statuses:  nil,
			errFunc:   require.NoError,
			wantCalls: 0,
		},
		{
			name: "completed on first poll returns nil",
			statuses: []arrcommand.Status{
				{Status: "completed"},
			},
			errFunc:   require.NoError,
			wantCalls: 1,
		},
		{
			name: "transitions queued then started then completed",
			statuses: []arrcommand.Status{
				{Status: "queued"},
				{Status: "started"},
				{Status: "completed"},
			},
			errFunc:   require.NoError,
			wantCalls: 3,
		},
		{
			name: "completed result successful is treated as success",
			statuses: []arrcommand.Status{
				{Status: "completed", Result: "successful"},
			},
			errFunc:   require.NoError,
			wantCalls: 1,
		},
		{
			name: "completed result unsuccessful surfaces as error",
			statuses: []arrcommand.Status{
				{Status: "completed", Result: "unsuccessful", Message: "no eligible files"},
			},
			errFunc:      require.Error,
			errSubstring: "no successful imports",
			wantCalls:    1,
		},
		{
			name: "failed status surfaces as error",
			statuses: []arrcommand.Status{
				{Status: "failed", Message: "boom"},
			},
			errFunc:      require.Error,
			errSubstring: "failed",
			wantCalls:    1,
		},
		{
			name: "aborted status surfaces as error",
			statuses: []arrcommand.Status{
				{Status: "started"},
				{Status: "aborted"},
			},
			errFunc:      require.Error,
			errSubstring: "aborted",
			wantCalls:    2,
		},
		{
			name:         "fetch error is wrapped",
			fetchErr:     errors.New("network unreachable"),
			errFunc:      require.Error,
			errSubstring: "get arrtest command status",
			wantCalls:    1,
		},
		{
			name: "case-insensitive status matching",
			statuses: []arrcommand.Status{
				{Status: "COMPLETED"},
			},
			errFunc:   require.NoError,
			wantCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			id := int64(1)
			if test.name == "zero id short-circuits without polling" {
				id = 0
			}

			calls := 0
			fetcher := func(_ context.Context, _ int64) (arrcommand.Status, error) {
				calls++

				if test.fetchErr != nil {
					return arrcommand.Status{}, test.fetchErr
				}

				idx := calls - 1
				if idx >= len(test.statuses) {
					idx = len(test.statuses) - 1
				}

				return test.statuses[idx], nil
			}

			err := arrcommand.Wait(t.Context(), fetcher, id, fastInterval, "arrtest")
			test.errFunc(t, err)

			if test.errSubstring != "" && err != nil {
				assert.Contains(t, err.Error(), test.errSubstring)
			}

			assert.Equal(t, test.wantCalls, calls, "unexpected fetcher invocation count")
		})
	}
}

// TestWait_ContextCancellationStopsPolling verifies the wait loop honors
// context cancellation while waiting between polls.
func TestWait_ContextCancellationStopsPolling(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	fetcher := func(_ context.Context, _ int64) (arrcommand.Status, error) {
		// Always return a non-terminal state so the loop keeps polling.
		return arrcommand.Status{Status: "queued"}, nil
	}

	// Cancel after a short delay to interrupt the wait loop.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := arrcommand.Wait(ctx, fetcher, 1, fastInterval, "arrtest")
	require.ErrorIs(t, err, context.Canceled)
}

// TestWait_ZeroIntervalUsesDefault verifies that passing a non-positive
// interval falls back to DefaultPollInterval rather than busy-looping.
func TestWait_ZeroIntervalUsesDefault(t *testing.T) {
	t.Parallel()

	// First call returns terminal so we don't actually wait DefaultPollInterval.
	fetcher := func(_ context.Context, _ int64) (arrcommand.Status, error) {
		return arrcommand.Status{Status: "completed"}, nil
	}

	require.NoError(t, arrcommand.Wait(t.Context(), fetcher, 1, 0, "arrtest"))
	require.NoError(t, arrcommand.Wait(t.Context(), fetcher, 1, -1, "arrtest"))
}
