//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---- directory management -----------------------------------------------

func resetDirs() error {
	for _, dir := range []string{
		filepath.Join(downloadsDir, "radarr"),
		filepath.Join(downloadsDir, "sonarr"),
		filepath.Join(processedDir, "radarr"),
		filepath.Join(processedDir, "radarr-library"),
		filepath.Join(processedDir, "sonarr"),
		filepath.Join(processedDir, "sonarr-library"),
		filepath.Join(configDir, "radarr"),
		filepath.Join(configDir, "sonarr"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	return nil
}

// ---- Docker Compose -----------------------------------------------------

func composeArgs(subcmd ...string) []string {
	args := []string{"compose", "-p", "e2e-media-processor", "-f", "docker-compose.yml"}
	return append(args, subcmd...)
}

// composeEnv returns the current process environment with UID and GID set to
// the running user's values. docker-compose.yml uses ${UID}:${GID} for the
// container user field; these variables are not exported by bash by default so
// they must be injected explicitly.
func composeEnv() []string {
	return append(os.Environ(),
		"UID="+strconv.Itoa(os.Getuid()),
		"GID="+strconv.Itoa(os.Getgid()),
	)
}

// composePull pulls all Docker images declared in docker-compose.yml, skipping
// any that are not available from a remote registry (e.g. locally-built images
// with pull_policy: never).
func composePull(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", composeArgs("pull", "--ignore-pull-failures")...)
	cmd.Env = composeEnv()
	stdout := newSlogWriter(slog.LevelInfo, "docker")
	stderr := newSlogWriter(slog.LevelWarn, "docker")
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	stdout.Flush()
	stderr.Flush()

	return err
}

func composeUp() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", composeArgs("up", "-d")...)
	cmd.Env = composeEnv()
	stdout := newSlogWriter(slog.LevelInfo, "docker")
	stderr := newSlogWriter(slog.LevelWarn, "docker")
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	stdout.Flush()
	stderr.Flush()

	return err
}

// composeUpWatcherWorker starts the watcher and worker containers (profile
// "app"), injecting the Hatchet client token into the environment so the
// compose interpolation of ${HATCHET_CLIENT_TOKEN} resolves correctly.
func composeUpWatcherWorker(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", composeArgs("--profile", "app", "up", "-d", "watcher", "worker")...)

	cmd.Env = append(composeEnv(), "HATCHET_CLIENT_TOKEN="+token)
	stdout := newSlogWriter(slog.LevelInfo, "docker")
	stderr := newSlogWriter(slog.LevelWarn, "docker")
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	stdout.Flush()
	stderr.Flush()

	return err
}

func composeDown() {
	cmd := exec.Command("docker", composeArgs("--profile", "app", "down", "-v", "--remove-orphans")...)
	cmd.Env = composeEnv()
	stdout := newSlogWriter(slog.LevelInfo, "docker")
	stderr := newSlogWriter(slog.LevelWarn, "docker")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	_ = cmd.Run()

	stdout.Flush()
	stderr.Flush()

	_ = os.RemoveAll(baseDir)
}

// waitForAppServices polls until the watcher and worker report ready via /readyz.
func waitForAppServices(ctx context.Context) error {
	type svc struct {
		name string
		fn   func() error
	}

	services := []svc{
		{"watcher", func() error { return checkHTTP(watcherHealthBase + "/readyz") }},
		{"worker", func() error { return checkHTTP(workerHealthBase + "/readyz") }},
	}

	for _, service := range services {
		log.Info("waiting for app service", "name", service.name)

		if err := pollUntil(ctx, 5*time.Second, service.fn); err != nil {
			return fmt.Errorf("%s not ready: %w", service.name, err)
		}

		log.Info("app service ready", "name", service.name)
	}

	return nil
}

// waitForServices polls until Radarr, Sonarr, and the Hatchet gRPC port are up.
func waitForServices(ctx context.Context) error {
	type svc struct {
		name string
		fn   func() error
	}

	services := []svc{
		{"radarr", func() error { return checkHTTP(radarrBase + "/ping") }},
		{"sonarr", func() error { return checkHTTP(sonarrBase + "/ping") }},
		{"hatchet-grpc", func() error { return checkTCP("localhost:7079") }},
	}

	for _, service := range services {
		log.Info("waiting for service", "name", service.name)

		if err := pollUntil(ctx, 5*time.Second, service.fn); err != nil {
			return fmt.Errorf("%s not ready: %w", service.name, err)
		}

		log.Info("service ready", "name", service.name)
	}

	return nil
}

func checkHTTP(url string) error {
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(url) //nolint:noctx // health poll, no caller context
	if err != nil {
		return err
	}

	_ = resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}

	return nil
}

func checkTCP(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}

	return conn.Close()
}

// ---- Hatchet token ------------------------------------------------------

// generateHatchetToken generates a long-lived Hatchet API token by running
// hatchet-admin inside the setup-config container. The default tenant ID
// (707d0855-80ab-4e1f-a156-f1c4546cbf52) seeded by the migration is used.
func generateHatchetToken() (string, error) {
	tokenCmd := exec.Command("docker", composeArgs(
		"run", "--no-deps", "--rm", "-T", "setup-config",
		"/hatchet/hatchet-admin", "token", "create",
		"--config", "/hatchet/config",
		"-e", "87600h",
	)...)
	tokenCmd.Env = composeEnv()

	tokenOut, err := tokenCmd.Output()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	// Extract the JWT token line (starts with "eyJ") in case log lines are
	// interleaved on stdout.
	for _, line := range strings.Split(string(tokenOut), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "eyJ") {
			return line, nil
		}
	}

	return "", fmt.Errorf("no JWT token found in hatchet-admin output")
}

// ---- polling helpers ----------------------------------------------------

// pollUntil calls cond repeatedly with interval spacing until it returns nil or
// ctx is done. It calls cond immediately on the first iteration. When ctx is
// cancelled or times out, the returned error wraps both ctx.Err() and the last
// error returned by cond so callers can see what was still failing at deadline.
func pollUntil(ctx context.Context, interval time.Duration, cond func() error) error {
	var lastErr error

	for {
		lastErr = cond()
		if lastErr == nil {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w; last condition error: %w", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}
