package watcherconfig

import (
	"fmt"
	"regexp"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"

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
	return &jsonschema.Schema{
		Type:        "string",
		Pattern:     sixFieldCronPattern,
		Description: "Hatchet 6-field cron expression (seconds-leading) controlling how often the watcher scans directories. Example: \"*/5 * * * * *\" runs every 5 seconds.",
	}
}

// CompiledRegexp is a Go regular expression that is compiled at YAML parse time. An invalid
// expression causes YAML unmarshaling (and therefore config loading) to fail with a descriptive
// error, so no separate validation step is required.
type CompiledRegexp struct {
	*regexp.Regexp
}

// JSONSchema returns a JSON Schema for CompiledRegexp. The type serialises as a scalar string
// (the regex pattern) at YAML parse time; reflecting the Go struct would produce an empty object
// schema, so this method overrides that to describe the on-disk shape accurately.
func (CompiledRegexp) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Format:      "regex",
		Description: "Go regular expression matched against the absolute path of each file or directory. A matching file is skipped; a matching directory prunes its entire subtree.",
	}
}

// UnmarshalYAML compiles the scalar YAML string as a Go regular expression. It returns an error
// if the node is not a scalar string, if the string is empty, or if the expression is invalid.
func (c *CompiledRegexp) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return fmt.Errorf("ignorePatterns entry must be a string, got YAML node kind %v (tag %s)", value.Kind, value.Tag)
	}

	if value.Value == "" {
		return fmt.Errorf("ignorePatterns entry must not be empty")
	}

	re, err := regexp.Compile(value.Value)
	if err != nil {
		return fmt.Errorf("invalid regular expression %q: %w", value.Value, err)
	}

	c.Regexp = re

	return nil
}

// validMediaTypes is the authoritative list of medialib.MediaType values accepted in config.
// It drives runtime validation; JSON Schema enum generation is handled by medialib.MediaType.JSONSchema.
var validMediaTypes = []medialib.MediaType{medialib.MovieType, medialib.ShowType}

// WatchEntry describes a watched location, mapping a filesystem path to a media type for dispatch
// and carrying a human-readable name for identification.
type WatchEntry struct {
	// Name is a human-readable label for this watch entry, used in logs and metrics.
	Name string `yaml:"name" jsonschema:"minLength=1" validate:"min=1"`
	// Path is the absolute filesystem path to the directory to watch.
	Path string `yaml:"path" jsonschema:"minLength=1" validate:"min=1"`
	// MediaType indicates whether this directory contains movies or TV show episodes.
	MediaType medialib.MediaType `yaml:"mediaType" validate:"mediatype"`
	// IgnorePatterns is an optional list of Go regular expressions matched against the absolute
	// path of each file and directory encountered during a scan. A matching file is silently
	// skipped; a matching directory causes its entire subtree to be pruned. Patterns are
	// compiled at config load; an invalid expression causes loadConfig to return an error.
	IgnorePatterns []CompiledRegexp `yaml:"ignorePatterns,omitempty" validate:"omitempty"`
	// PreserveSource controls whether the source file is deleted after successful processing.
	// When true, the source file is kept; when false or omitted, the source file is deleted
	// (default behaviour).
	PreserveSource bool `yaml:"preserveSource,omitempty"`
	// RetainEmptyDirectories controls whether empty parent directories are removed after the
	// source file is deleted. When false or omitted (the default), parent directories that
	// become empty as a result of source-file deletion are pruned bottom-up, stopping at the
	// watch root. When true, no directory removal is performed (current behaviour).
	RetainEmptyDirectories bool `yaml:"retainEmptyDirectories,omitempty"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// CronSchedule controls how often the watcher scans directories (default: every 5 seconds).
	// Uses Hatchet's 6-field cron format with a leading seconds field, e.g. "*/5 * * * * *".
	CronSchedule CronExpression `yaml:"cronSchedule,omitempty" validate:"omitempty,hatchetcron"`
	// Watches lists the directories to monitor and their associated media types.
	Watches []WatchEntry `yaml:"watches,omitempty" validate:"dive"`
}

// SetDefaults implements defaults.Setter to initialize Config fields from package constants,
// avoiding duplicate literal values in struct tags.
func (cfg *Config) SetDefaults() {
	if cfg.CronSchedule == "" {
		cfg.CronSchedule = DefaultCronSchedule
	}
}
