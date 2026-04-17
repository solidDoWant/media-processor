package watcherconfig

import (
	"github.com/invopop/jsonschema"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

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

// validMediaTypes is the authoritative list of medialib.MediaType values accepted in config.
// It drives runtime validation; JSON Schema enum generation is handled by medialib.MediaType.JSONSchema.
var validMediaTypes = []medialib.MediaType{medialib.MovieType, medialib.ShowType}

// WatchEntry describes a watched location, mapping a filesystem path to a media type for dispatch
// and carrying a human-readable name for identification.
type WatchEntry struct {
	Name string `yaml:"name" jsonschema:"minLength=1" validate:"min=1"`
	Path string `yaml:"path" jsonschema:"minLength=1" validate:"min=1"`
	// MediaType must be one of the values in validMediaTypes; validated by the mediatype tag.
	// The validate tag is required for runtime enforcement; medialib.MediaType.JSONSchema handles schema generation.
	MediaType medialib.MediaType `yaml:"media_type" validate:"mediatype"`
	// IgnorePatterns is an optional list of Go regular expressions matched against the absolute
	// path of each file and directory encountered during a scan. A matching file is silently
	// skipped; a matching directory causes its entire subtree to be pruned. Patterns are
	// validated at config load; an invalid expression causes loadConfig to return an error.
	IgnorePatterns []string `yaml:"ignorePatterns,omitempty" validate:"omitempty,dive,validregex"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// CronSchedule is the Hatchet cron expression controlling how often the watcher
	// scans directories (default: every 5 seconds). Supports Hatchet's 6-field format
	// with a leading seconds field, e.g. "*/5 * * * * *".
	// The CronExpression type provides the JSON Schema pattern; hatchetcron validates at runtime.
	CronSchedule CronExpression `yaml:"cron_schedule,omitempty" validate:"omitempty,hatchetcron"`
	// Watches uses validate:"dive" to validate each WatchEntry element in the slice.
	Watches []WatchEntry `yaml:"watches,omitempty" validate:"dive"`
}

// SetDefaults implements defaults.Setter to initialize Config fields from package constants,
// avoiding duplicate literal values in struct tags.
func (cfg *Config) SetDefaults() {
	if cfg.CronSchedule == "" {
		cfg.CronSchedule = DefaultCronSchedule
	}
}
