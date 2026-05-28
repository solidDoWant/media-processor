package watcherconfig

import (
	"fmt"
	"regexp"
	"time"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"

	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// DefaultScanInterval is the duration between directory scans when none is specified in the config.
const DefaultScanInterval = Interval(5 * time.Second)

// Interval is a Go duration parsed from a YAML scalar like "5s" or "1m30s". It exists because
// yaml.v3 does not natively understand time.Duration values.
type Interval time.Duration

// Duration returns the interval as a time.Duration for use with time.NewTicker etc.
func (i Interval) Duration() time.Duration { return time.Duration(i) }

// String returns the canonical Go duration representation of the interval.
func (i Interval) String() string { return time.Duration(i).String() }

// JSONSchema returns a JSON Schema describing the on-disk shape of Interval (a duration string).
func (Interval) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Description: "Go duration string (e.g. \"5s\", \"1m30s\") between directory scans. Defaults to \"5s\" when omitted.",
	}
}

// UnmarshalYAML parses a YAML scalar string as a Go duration. An explicit
// non-positive duration is rejected so it cannot silently fall back to the
// default in SetDefaults.
func (i *Interval) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("scanInterval must be a string, got YAML node kind %v", value.Kind)
	}

	d, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid scanInterval %q: %w", value.Value, err)
	}

	if d <= 0 {
		return fmt.Errorf("scanInterval must be positive, got %q", value.Value)
	}

	*i = Interval(d)

	return nil
}

// MarshalYAML emits the interval as its canonical Go duration string.
func (i Interval) MarshalYAML() (any, error) {
	return time.Duration(i).String(), nil
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

// WatchEntryInput describes the input source for a watch entry.
type WatchEntryInput struct {
	// Path is the filesystem path to the directory the watcher scans. Relative paths are
	// resolved against the watcher's working directory.
	Path string `yaml:"path" jsonschema:"minLength=1"`
}

// WatchEntryOutput describes the output destination for a watch entry.
type WatchEntryOutput struct {
	// Path is the filesystem path to the directory where processed files are written.
	Path string `yaml:"path" jsonschema:"minLength=1"`
	// RemotePath is the path by which the output directory is known to the arr service (Radarr/Sonarr).
	// Set this when the worker and the arr service mount the output volume at different paths.
	// When empty, no path translation is applied.
	RemotePath string `yaml:"remotePath,omitempty"`
}

// WatchEntry describes a watched location, mapping a filesystem path to a media type for dispatch
// and carrying a human-readable name for identification.
type WatchEntry struct {
	// Name is a human-readable label for this watch entry, used in logs and metrics.
	Name string `yaml:"name" jsonschema:"minLength=1"`
	// Input describes the directory the watcher scans for media files.
	Input WatchEntryInput `yaml:"input"`
	// MediaType indicates whether this directory contains movies or TV show episodes.
	MediaType medialib.MediaType `yaml:"mediaType" validate:"mediatype"`
	// IgnorePatterns is an optional list of Go regular expressions matched against the absolute
	// path of each file and directory encountered during a scan. A matching file is silently
	// skipped; a matching directory causes its entire subtree to be pruned. Patterns are
	// compiled when the configuration is loaded; an invalid expression causes configuration loading to fail.
	IgnorePatterns []CompiledRegexp `yaml:"ignorePatterns,omitempty" validate:"omitempty"`
	// PreserveSource controls whether the source file is deleted after successful processing.
	// When true, the source file is kept; when false or omitted, the source file is deleted
	// (default behaviour).
	PreserveSource bool `yaml:"preserveSource,omitempty"`
	// RetainEmptyDirectories controls whether empty parent directories are removed after the
	// source file is deleted. When false or omitted (the default), parent directories that
	// become empty as a result of source-file deletion are pruned bottom-up, stopping at the
	// watch root. When true, no directory removal is performed.
	RetainEmptyDirectories bool `yaml:"retainEmptyDirectories,omitempty"`
	// SkipCropDetection disables the crop-detection step for files from this watch. When true,
	// the workflow does not run the detect-crop activity and transcodes the full frame without
	// a crop filter. When false or omitted (the default), crop detection runs as normal.
	SkipCropDetection bool `yaml:"skipCropDetection,omitempty"`
	// Output describes where processed files are written and how the output path is translated
	// for the arr service.
	Output WatchEntryOutput `yaml:"output"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// ScanInterval controls how often the watcher walks each configured watch directory.
	// Specified as a Go duration string (e.g. "5s", "1m30s"). Defaults to 5 seconds when omitted.
	ScanInterval Interval `yaml:"scanInterval,omitempty" validate:"gt=0"`
	// Watches lists the directories to monitor and their associated media types.
	Watches []WatchEntry `yaml:"watches,omitempty" validate:"dive"`
}

// SetDefaults implements defaults.Setter to initialize Config fields from package constants,
// avoiding duplicate literal values in struct tags.
func (cfg *Config) SetDefaults() {
	if cfg.ScanInterval == 0 {
		cfg.ScanInterval = DefaultScanInterval
	}
}
