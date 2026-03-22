// Package webhook provides an HTTP client for posting failure notifications
// to a configured webhook endpoint.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultTimeout = 10 * time.Second

// FailureEvent describes a workflow step failure.
type FailureEvent struct {
	Workflow string
	FilePath string
	Step     string
	Err      error
}

// PayloadFunc builds the HTTP request body for a given failure event.
// Implementations can return any JSON structure — for example, Discord's
// {"content": "..."} format.
type PayloadFunc func(event FailureEvent) ([]byte, error)

// Client sends failure notifications to a webhook endpoint.
type Client struct {
	URL          string
	Timeout      time.Duration // zero → defaultTimeout (10s)
	BuildPayload PayloadFunc   // nil → DefaultPayload
}

// defaultPayload is the built-in payload shape described in the issue spec.
type defaultPayloadShape struct {
	Workflow string `json:"workflow"`
	FilePath string `json:"file_path"`
	Step     string `json:"step"`
	Error    string `json:"error"`
}

// DefaultPayload marshals the event into the standard JSON shape:
//
//	{"workflow":"...","file_path":"...","step":"...","error":"..."}
func DefaultPayload(e FailureEvent) ([]byte, error) {
	errMsg := ""
	if e.Err != nil {
		errMsg = e.Err.Error()
	}
	return json.Marshal(defaultPayloadShape{
		Workflow: e.Workflow,
		FilePath: e.FilePath,
		Step:     e.Step,
		Error:    errMsg,
	})
}

// NotifyFailure sends an HTTP POST to c.URL with the failure event payload.
// If c.URL is empty the call is a no-op and nil is returned.
// If the endpoint returns a non-2xx status code, a non-nil error is returned
// containing the status code.
func (c *Client) NotifyFailure(ctx context.Context, event FailureEvent) error {
	if c.URL == "" {
		return nil
	}

	buildPayload := c.BuildPayload
	if buildPayload == nil {
		buildPayload = DefaultPayload
	}

	body, err := buildPayload(event)
	if err != nil {
		return fmt.Errorf("webhook: build payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	httpClient := &http.Client{Timeout: timeout}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: unexpected status %d", resp.StatusCode)
	}

	return nil
}
