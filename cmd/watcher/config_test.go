package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	// AC3: Given a config file mapping a directory to a workflow name,
	// when mediaprocessor-watcher starts, then it loads the mappings without error.
	content := `
watches:
  - path: /watch/movies
    workflow: MovieWorkflow
  - path: /watch/shows
    workflow: ShowWorkflow
`
	path := writeTempConfig(t, content)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("expected no error loading valid config, got: %v", err)
	}

	if len(cfg.Watches) != 2 {
		t.Fatalf("expected 2 watch entries, got %d", len(cfg.Watches))
	}

	if cfg.Watches[0].Path != "/watch/movies" || cfg.Watches[0].Workflow != "MovieWorkflow" {
		t.Errorf("unexpected first entry: %+v", cfg.Watches[0])
	}

	if cfg.Watches[1].Path != "/watch/shows" || cfg.Watches[1].Workflow != "ShowWorkflow" {
		t.Errorf("unexpected second entry: %+v", cfg.Watches[1])
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	// AC4: Given a missing config file path,
	// when mediaprocessor-watcher starts, then it exits non-zero with a descriptive error message.
	_, err := loadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing config file, got nil")
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	// AC4: Given an invalid config file,
	// when mediaprocessor-watcher starts, then it exits non-zero with a descriptive error message.
	path := writeTempConfig(t, "{ this is: [not valid yaml")

	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}
