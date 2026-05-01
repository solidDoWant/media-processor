package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/creasty/defaults"
	"gopkg.in/yaml.v3"

	"github.com/solidDoWant/media-processor/internal/watcherconfig"
)

// Type aliases so package-level tests and code continue to reference Config/WatchEntry/CompiledRegexp directly.
type Config = watcherconfig.Config
type WatchEntry = watcherconfig.WatchEntry
type CompiledRegexp = watcherconfig.CompiledRegexp

// loadConfig reads and parses the watcher YAML config file at path. Decoding is strict:
// any unknown top-level field (for example the removed "cronSchedule") fails loading with
// a clear error so legacy configs do not silently default.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %q: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config file %q: %w", path, err)
	}

	if err := defaults.Set(&cfg); err != nil {
		return nil, fmt.Errorf("cannot set config defaults: %w", err)
	}

	if err := watcherconfig.NewValidator().Struct(cfg); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}

	return &cfg, nil
}
