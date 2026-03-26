package watcherconfig

// DefaultCronSchedule is the Hatchet cron expression used when none is specified in the config.
const DefaultCronSchedule = "*/5 * * * * *"

// WorkflowName identifies a Hatchet workflow that the watcher can trigger.
type WorkflowName string

const (
	// MovieWorkflow is the workflow for processing movie files.
	MovieWorkflow WorkflowName = "MovieWorkflow"
	// ShowWorkflow is the workflow for processing TV show files.
	ShowWorkflow WorkflowName = "ShowWorkflow"
)

// WatchEntry maps a filesystem path to a Hatchet workflow name.
type WatchEntry struct {
	Path     string       `yaml:"path"     jsonschema:"required,minLength=1"                         validate:"required"`
	Workflow WorkflowName `yaml:"workflow" jsonschema:"required,enum=MovieWorkflow,enum=ShowWorkflow" validate:"required,oneof=MovieWorkflow ShowWorkflow"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// CronSchedule is the Hatchet cron expression controlling how often the watcher
	// scans directories (default: every 5 seconds). Supports Hatchet's 6-field format
	// with a leading seconds field, e.g. "*/5 * * * * *".
	CronSchedule string       `yaml:"cron_schedule" jsonschema:"pattern=^(\\S+ ){5}\\S+$" validate:"omitempty,cron"`
	Watches      []WatchEntry `yaml:"watches"       jsonschema:"required"                  validate:"dive"`
}
