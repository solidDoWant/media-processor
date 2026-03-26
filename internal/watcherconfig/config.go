package watcherconfig

import "github.com/invopop/jsonschema"

// DefaultCronSchedule is the Hatchet cron expression used when none is specified in the config.
const DefaultCronSchedule = "*/5 * * * * *"

// WorkflowName identifies a Hatchet workflow that the watcher can trigger.
type WorkflowName string

const (
	// movie is the workflow for processing movie files.
	movie WorkflowName = "MovieWorkflow"
	// show is the workflow for processing TV show files.
	show WorkflowName = "ShowWorkflow"
)

// validWorkflowNames is the authoritative list of WorkflowName values accepted in config.
// It drives both JSON Schema enum generation and runtime validation.
var validWorkflowNames = []WorkflowName{movie, show}

// JSONSchema returns a JSON Schema for WorkflowName derived from validWorkflowNames,
// so enum values are defined in one place rather than duplicated in struct tags.
func (WorkflowName) JSONSchema() *jsonschema.Schema {
	enum := make([]any, len(validWorkflowNames))
	for i, v := range validWorkflowNames {
		enum[i] = string(v)
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}

// WatchEntry maps a filesystem path to a Hatchet workflow name.
type WatchEntry struct {
	Path string `yaml:"path" jsonschema:"required,minLength=1" validate:"min=1"`
	// Workflow must be one of the values in validWorkflowNames; validated by the workflowname tag.
	Workflow WorkflowName `yaml:"workflow" jsonschema:"required" validate:"workflowname"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// CronSchedule is the Hatchet cron expression controlling how often the watcher
	// scans directories (default: every 5 seconds). Supports Hatchet's 6-field format
	// with a leading seconds field, e.g. "*/5 * * * * *".
	CronSchedule string `yaml:"cron_schedule" default:"*/5 * * * * *" jsonschema:"pattern=^(\\S+ ){5}\\S+$" validate:"omitempty,hatchetcron"`
	// Watches uses validate:"dive" to validate each WatchEntry element in the slice.
	Watches []WatchEntry `yaml:"watches" jsonschema:"required" validate:"dive"`
}
