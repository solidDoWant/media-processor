package shared

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/solidDoWant/media-processor/pkg/webhook"
)

// NotifyWorkflowFailure sends an aggregated failure notification to webhookClient.
// It is intended to be called from a workflow's OnFailure handler.
// workflowName is included in the webhook payload; filePath is the file being processed.
// Returns nil (no-op) when there are no step errors.
func NotifyWorkflowFailure(ctx hatchet.Context, workflowName, filePath string, webhookClient *webhook.Client) error {
	stepErrors := ctx.StepRunErrors()
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
