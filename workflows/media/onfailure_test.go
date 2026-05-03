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
		step       string
		errMsg     string
		wantCalled bool
		wantStep   string
		wantErrMsg string
		errFunc    require.ErrorAssertionFunc
	}{
		{
			name:       "empty step is a no-op",
			step:       "",
			errMsg:     "",
			wantCalled: false,
			errFunc:    require.NoError,
		},
		{
			name:       "single step error sends correct payload",
			step:       "transcode",
			errMsg:     "ffmpeg exited with code 1",
			wantCalled: true,
			wantStep:   "transcode",
			wantErrMsg: "transcode: ffmpeg exited with code 1",
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
			err := NotifyWorkflowFailure(t.Context(), tt.step, tt.errMsg, "TestWorkflow", "/media/file.mkv", client)

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
	err := NotifyWorkflowFailure(t.Context(), "lookup", "not found", "TestWorkflow", "/media/file.mkv", client)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notify failure")
}
