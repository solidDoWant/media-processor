package media

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/pkg/webhook"
)

func TestNotifyWorkflowFailure(t *testing.T) {
	tests := []struct {
		name       string
		stepErrors map[string]string
		wantCalled bool
		wantStep   string
		wantErrMsg string
		errFunc    require.ErrorAssertionFunc
	}{
		{
			name:       "empty step errors is a no-op",
			stepErrors: map[string]string{},
			wantCalled: false,
			errFunc:    require.NoError,
		},
		{
			name:       "nil step errors is a no-op",
			stepErrors: nil,
			wantCalled: false,
			errFunc:    require.NoError,
		},
		{
			name:       "single step error sends correct payload",
			stepErrors: map[string]string{"transcode": "ffmpeg exited with code 1"},
			wantCalled: true,
			wantStep:   "transcode",
			wantErrMsg: "transcode: ffmpeg exited with code 1",
			errFunc:    require.NoError,
		},
		{
			name: "multiple step errors are sorted and joined deterministically",
			stepErrors: map[string]string{
				"lookup":    "not found",
				"transcode": "disk full",
				"cleanup":   "permission denied",
			},
			wantCalled: true,
			wantStep:   "cleanup, lookup, transcode",
			wantErrMsg: "cleanup: permission denied\nlookup: not found\ntranscode: disk full",
			errFunc:    require.NoError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false

			var gotPayload map[string]string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true

				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotPayload))
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			client := &webhook.Client{URL: srv.URL}
			err := NotifyWorkflowFailure(t.Context(), tt.stepErrors, "TestWorkflow", "/media/file.mkv", client)

			tt.errFunc(t, err)
			assert.Equal(t, tt.wantCalled, called, "webhook server call expectation")

			if tt.wantCalled {
				assert.Equal(t, tt.wantStep, gotPayload["step"])
				assert.Equal(t, tt.wantErrMsg, gotPayload["error"])
				assert.Equal(t, "TestWorkflow", gotPayload["workflow"])
				assert.Equal(t, "/media/file.mkv", gotPayload["file_path"])
			}
		})
	}
}

func TestNotifyWorkflowFailure_WebhookErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := &webhook.Client{URL: srv.URL}
	err := NotifyWorkflowFailure(t.Context(), map[string]string{"lookup": "not found"}, "TestWorkflow", "/media/file.mkv", client)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notify failure")
}
