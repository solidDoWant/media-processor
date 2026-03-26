package watcherconfig

import "github.com/invopop/jsonschema"

// DefaultCronSchedule is the Hatchet cron expression used when none is specified in the config.
const DefaultCronSchedule CronExpression = "*/5 * * * * *"

// CronExpression is a Hatchet 6-field cron expression (seconds-leading, e.g. "*/5 * * * * *").
// It implements JSONSchema to embed the pattern constraint in the generated schema, keeping the
// regex defined in validate.go as the single source of truth for both schema and runtime checks.
type CronExpression string

// JSONSchema returns a JSON Schema for CronExpression using the canonical Hatchet cron regex.
func (CronExpression) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: sixFieldCronPattern}
}

// WorkflowName identifies a Hatchet workflow that the watcher can trigger.
type WorkflowName string

const (
	// MovieWorkflow is the workflow for processing movie files.
	MovieWorkflow WorkflowName = "movie"
	// ShowWorkflow is the workflow for processing TV show files.
	ShowWorkflow WorkflowName = "show"
)

// validWorkflowNames is the authoritative list of WorkflowName values accepted in config.
// It drives both JSON Schema enum generation and runtime validation.
var validWorkflowNames = []WorkflowName{MovieWorkflow, ShowWorkflow}

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
	Path string `yaml:"path" jsonschema:"minLength=1" validate:"min=1"`
	// Workflow must be one of the values in validWorkflowNames; validated by the workflowname tag.
	// The validate tag is required for runtime enforcement; JSONSchema() handles schema generation.
	Workflow WorkflowName `yaml:"workflow" validate:"workflowname"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// CronSchedule is the Hatchet cron expression controlling how often the watcher
	// scans directories (default: every 5 seconds). Supports Hatchet's 6-field format
	// with a leading seconds field, e.g. "*/5 * * * * *".
	// The CronExpression type provides the JSON Schema pattern; hatchetcron validates at runtime.
	CronSchedule CronExpression `yaml:"cron_schedule,omitempty" validate:"omitempty,hatchetcron"`
	// Watches uses validate:"dive" to validate each WatchEntry element in the slice.
	Watches []WatchEntry `yaml:"watches" validate:"dive"`
}

// SetDefaults implements defaults.Setter to initialize Config fields from package constants,
// avoiding duplicate literal values in struct tags.
func (cfg *Config) SetDefaults() {
	if cfg.CronSchedule == "" {
		cfg.CronSchedule = DefaultCronSchedule
	}
}
