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

// MediaType identifies the kind of media file a watch entry handles.
type MediaType string

const (
	// Movie is the media type for movie files.
	Movie MediaType = "movie"
	// Show is the media type for TV show episode files.
	Show MediaType = "show"
)

// validMediaTypes is the authoritative list of MediaType values accepted in config.
// It drives both JSON Schema enum generation and runtime validation.
var validMediaTypes = []MediaType{Movie, Show}

// JSONSchema returns a JSON Schema for MediaType derived from validMediaTypes,
// so enum values are defined in one place rather than duplicated in struct tags.
func (MediaType) JSONSchema() *jsonschema.Schema {
	enum := make([]any, len(validMediaTypes))
	for i, v := range validMediaTypes {
		enum[i] = string(v)
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}

// WatchEntry maps a filesystem path to a media type for dispatch.
type WatchEntry struct {
	Path string `yaml:"path" jsonschema:"minLength=1" validate:"min=1"`
	// MediaType must be one of the values in validMediaTypes; validated by the mediatype tag.
	// The validate tag is required for runtime enforcement; JSONSchema() handles schema generation.
	MediaType MediaType `yaml:"media_type" validate:"mediatype"`
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
