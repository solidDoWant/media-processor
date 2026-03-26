package main

import (
	"fmt"
	"os"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"

	"github.com/solidDoWant/media-processor/internal/watcherconfig"
)

// Type aliases so package-level tests and code continue to reference Config/WatchEntry directly.
type Config = watcherconfig.Config
type WatchEntry = watcherconfig.WatchEntry

// loadConfig reads and parses the watcher YAML config file at path.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %q: %w", path, err)
	}

	cfg := Config{CronSchedule: watcherconfig.DefaultCronSchedule}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config file %q: %w", path, err)
	}

	if err := validator.New(validator.WithRequiredStructEnabled()).Struct(cfg); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}

	return &cfg, nil
}
