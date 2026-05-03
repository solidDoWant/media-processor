package media

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// NotifyWorkflowFailure sends a single-step failure notification to webhookClient.
// step is the failing activity name; errMsg is the underlying error message.
// workflowName is included in the webhook payload; filePath is the file being processed.
// Returns nil (no-op) when step is empty.
func NotifyWorkflowFailure(ctx context.Context, step, errMsg, workflowName, filePath string, webhookClient *webhook.Client) error {
	if step == "" {
		slog.InfoContext(ctx, "no step error, skipping failure notification", slog.String("file", filePath))
		return nil
	}

	if webhookClient.URL == "" {
		slog.InfoContext(ctx, "no webhook URL configured, skipping failure notification", slog.String("file", filePath))
		return nil
	}

	if err := webhookClient.NotifyFailure(ctx, webhook.FailureEvent{
		Workflow: workflowName,
		FilePath: filePath,
		Step:     step,
		Err:      fmt.Errorf("%s: %s", step, errMsg),
	}); err != nil {
		return fmt.Errorf("notify failure: %w", err)
	}

	slog.InfoContext(ctx, "failure notification sent", slog.String("file", filePath))

	return nil
}
