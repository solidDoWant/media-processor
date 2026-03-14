package workflows

import hatchet "github.com/hatchet-dev/hatchet/sdks/go"

// placeholderInput is the no-op input type for the placeholder workflow.
type placeholderInput struct{}

// placeholderOutput is the no-op output type for the placeholder workflow.
type placeholderOutput struct{}

// NewPlaceholder returns a standalone no-op task used to verify Hatchet connectivity.
// It accepts no meaningful input and performs no work.
func NewPlaceholder(client *hatchet.Client) *hatchet.StandaloneTask {
	return client.NewStandaloneTask(
		"placeholder",
		func(_ hatchet.Context, _ placeholderInput) (placeholderOutput, error) {
			return placeholderOutput{}, nil
		},
	)
}
