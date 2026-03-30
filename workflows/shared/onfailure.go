package shared

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// NotifyWorkflowFailure sends an aggregated failure notification to webhookClient.
// stepErrors is the map returned by ctx.StepRunErrors() in an OnFailure handler.
// workflowName is included in the webhook payload; filePath is the file being processed.
// Returns nil (no-op) when stepErrors is empty.
func NotifyWorkflowFailure(ctx context.Context, stepErrors map[string]string, workflowName, filePath string, webhookClient *webhook.Client) error {
	if len(stepErrors) == 0 {
		return nil
	}

	steps := make([]string, 0, len(stepErrors))
	for stepName := range stepErrors {
		steps = append(steps, stepName)
	}

	sort.Strings(steps)

	errs := make([]error, 0, len(stepErrors))
	for _, stepName := range steps {
		errs = append(errs, fmt.Errorf("%s: %s", stepName, stepErrors[stepName]))
	}

	if err := webhookClient.NotifyFailure(ctx, webhook.FailureEvent{
		Workflow: workflowName,
		FilePath: filePath,
		Step:     strings.Join(steps, ", "),
		Err:      errors.Join(errs...),
	}); err != nil {
		return fmt.Errorf("notify failure: %w", err)
	}

	return nil
}
