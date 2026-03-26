package main

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/solidDoWant/media-processor/internal/watcherconfig"
)

// Type aliases so package-level tests and code continue to reference Config/WatchEntry directly.
type Config = watcherconfig.Config
type WatchEntry = watcherconfig.WatchEntry

const defaultCronSchedule = watcherconfig.DefaultCronSchedule

// sixFieldCron matches a cron expression with exactly 6 space-separated fields.
var sixFieldCron = regexp.MustCompile(`^(\S+ ){5}\S+$`)

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

	for i, w := range cfg.Watches {
		if w.Path == "" {
			return nil, fmt.Errorf("watch entry %d: path must not be empty", i)
		}
		if w.Workflow == "" {
			return nil, fmt.Errorf("watch entry %d: workflow must not be empty", i)
		}
	}

	if cfg.CronSchedule != "" && !sixFieldCron.MatchString(cfg.CronSchedule) {
		return nil, fmt.Errorf("cron_schedule %q is not a valid 6-field cron expression (e.g. \"*/5 * * * * *\")", cfg.CronSchedule)
	}

	return &cfg, nil
}
