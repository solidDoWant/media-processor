package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// WatchEntry maps a filesystem path to a Hatchet workflow name.
type WatchEntry struct {
	Path     string `yaml:"path"`
	Workflow string `yaml:"workflow"`
}

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	Watches []WatchEntry `yaml:"watches"`
}

// loadConfig reads and parses the watcher YAML config file at path.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config file %q: %w", path, err)
	}

	return &cfg, nil
}
