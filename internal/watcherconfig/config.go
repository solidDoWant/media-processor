package watcherconfig

// DefaultCronSchedule is the Hatchet cron expression used when none is specified in the config.
const DefaultCronSchedule = "*/5 * * * * *"

// WatchEntry maps a filesystem path to a Hatchet workflow name.
type WatchEntry struct {
	Path     string `yaml:"path"     jsonschema:"required,minLength=1"`
	Workflow string `yaml:"workflow" jsonschema:"required,minLength=1"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// CronSchedule is the Hatchet cron expression controlling how often the watcher
	// scans directories (default: every 5 seconds). Supports Hatchet's 6-field format
	// with a leading seconds field, e.g. "*/5 * * * * *".
	CronSchedule string       `yaml:"cron_schedule" jsonschema:"pattern=^(\\S+ ){5}\\S+$"`
	Watches      []WatchEntry `yaml:"watches"       jsonschema:"required"`
}
