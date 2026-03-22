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

const defaultCronSchedule = "*/5 * * * * *"

// Config is the top-level watcher configuration loaded from the YAML config file.
type Config struct {
	// CronSchedule is the Hatchet cron expression controlling how often the watcher
	// scans directories (default: every 5 seconds). Supports Hatchet's 6-field format
	// with a leading seconds field, e.g. "*/5 * * * * *".
	CronSchedule string       `yaml:"cron_schedule"`
	Watches      []WatchEntry `yaml:"watches"`
}

// loadConfig reads and parses the watcher YAML config file at path.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %q: %w", path, err)
	}

	cfg := Config{CronSchedule: defaultCronSchedule}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config file %q: %w", path, err)
	}

	return &cfg, nil
}
