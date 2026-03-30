package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/webhook"
)

var testEvent = webhook.FailureEvent{
	Workflow: "MovieWorkflow",
	FilePath: "/watch/movies/example.mkv",
	Step:     "transcode",
	Err:      errors.New("exit status 1"),
}

// TestNotifyFailure_DefaultPayloadContents verifies the POST body contains all
// required fields when using the default payload builder.
func TestNotifyFailure_DefaultPayloadContents(t *testing.T) {
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var err error

		gotBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &webhook.Client{URL: srv.URL}
	err := client.NotifyFailure(t.Context(), testEvent)
	require.NoError(t, err)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	assert.Equal(t, "MovieWorkflow", payload["workflow"])
	assert.Equal(t, "/watch/movies/example.mkv", payload["file_path"])
	assert.Equal(t, "transcode", payload["step"])
	assert.Equal(t, "exit status 1", payload["error"])
}

// TestNotifyFailure_StatusCodes verifies return values for 2xx and non-2xx responses.
func TestNotifyFailure_StatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		errFunc    require.ErrorAssertionFunc
		errMsg     string
	}{
		{
			name:       "2xx returns nil",
			statusCode: http.StatusOK,
			errFunc:    require.NoError,
		},
		{
			name:       "2xx created returns nil",
			statusCode: http.StatusCreated,
			errFunc:    require.NoError,
		},
		{
			name:       "non-2xx returns error with status code",
			statusCode: http.StatusInternalServerError,
			errFunc:    require.Error,
			errMsg:     "500",
		},
		{
			name:       "4xx returns error with status code",
			statusCode: http.StatusBadRequest,
			errFunc:    require.Error,
			errMsg:     "400",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			client := &webhook.Client{URL: srv.URL}
			err := client.NotifyFailure(t.Context(), testEvent)

			if tc.errFunc == nil {
				tc.errFunc = require.NoError
			}

			tc.errFunc(t, err)

			if tc.errMsg != "" {
				assert.ErrorContains(t, err, tc.errMsg)
			}
		})
	}
}

// TestNotifyFailure_EmptyURL verifies that an empty URL is a no-op.
func TestNotifyFailure_EmptyURL(t *testing.T) {
	client := &webhook.Client{URL: ""}
	err := client.NotifyFailure(t.Context(), testEvent)
	require.NoError(t, err)
}

// TestNotifyFailure_CancelledContext verifies that a cancelled context causes
// the call to return promptly with a context error.
func TestNotifyFailure_CancelledContext(t *testing.T) {
	// Server that blocks so we can test cancellation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before calling

	client := &webhook.Client{URL: srv.URL}
	err := client.NotifyFailure(ctx, testEvent)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestNotifyFailure_UnreachableEndpoint verifies that an unreachable endpoint
// returns a non-nil error.
func TestNotifyFailure_UnreachableEndpoint(t *testing.T) {
	// Port 1 is reserved and connections to it are reliably refused.
	client := &webhook.Client{URL: "http://127.0.0.1:1"}
	err := client.NotifyFailure(t.Context(), testEvent)
	require.Error(t, err)
}

// TestNotifyFailure_CustomPayloadFunc verifies that a custom PayloadFunc is used
// instead of the default, enabling arbitrary endpoint formats such as Discord.
func TestNotifyFailure_CustomPayloadFunc(t *testing.T) {
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error

		gotBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	customPayload := []byte(`{"content":"custom discord message"}`)
	client := &webhook.Client{
		URL: srv.URL,
		BuildPayload: func(e webhook.FailureEvent) ([]byte, error) {
			return customPayload, nil
		},
	}

	err := client.NotifyFailure(t.Context(), testEvent)
	require.NoError(t, err)
	assert.Equal(t, customPayload, gotBody)
}

// TestDefaultPayload verifies the standalone DefaultPayload function produces
// the expected JSON structure.
func TestDefaultPayload(t *testing.T) {
	data, err := webhook.DefaultPayload(testEvent)
	require.NoError(t, err)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(data, &payload))
	assert.Equal(t, "MovieWorkflow", payload["workflow"])
	assert.Equal(t, "/watch/movies/example.mkv", payload["file_path"])
	assert.Equal(t, "transcode", payload["step"])
	assert.Equal(t, "exit status 1", payload["error"])
}
