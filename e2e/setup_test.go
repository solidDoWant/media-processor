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

// Package-level state for the watcher and worker subprocesses.
var (
	watcherCmd *exec.Cmd
	workerCmd  *exec.Cmd
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

// composePull pulls all Docker images declared in docker-compose.yml.
// It uses ctx to enforce a timeout for potentially large image downloads.
func composePull(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", composeArgs("pull")...)
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

func composeDown() {
	cmd := exec.Command("docker", composeArgs("down", "-v", "--remove-orphans")...)
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

func generateHatchetToken() (string, error) {
	// Query the tenant ID from the e2e postgres container.
	tenantCmd := exec.Command("docker", composeArgs(
		"exec", "-T", "postgres",
		"psql", "-U", "hatchet", "-d", "hatchet", "-t", "-c",
		`SELECT id FROM "Tenant" WHERE slug = 'default' LIMIT 1`,
	)...)
	tenantCmd.Env = composeEnv()

	tenantOut, err := tenantCmd.Output()
	if err != nil {
		return "", fmt.Errorf("query tenant ID: %w", err)
	}

	tenantID := strings.TrimSpace(string(tenantOut))

	if tenantID == "" {
		return "", fmt.Errorf("empty tenant ID from postgres")
	}

	// Generate the token using the setup-config container.
	tokenCmd := exec.Command("docker", composeArgs(
		"run", "--no-deps", "--rm", "-T", "setup-config",
		"/hatchet/hatchet-admin", "token", "create",
		"--config", "/hatchet/config",
		"--tenant-id", tenantID,
	)...)
	tokenCmd.Env = composeEnv()

	tokenOut, err := tokenCmd.Output()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	token := strings.TrimSpace(string(tokenOut))

	if token == "" {
		return "", fmt.Errorf("empty token from hatchet-admin")
	}

	return token, nil
}

// ---- watcher + worker subprocesses --------------------------------------

func startProcesses() error {
	root := moduleRoot()

	// Build watcher and worker using the Makefile target.
	buildCmd := exec.Command("make", "build")
	buildCmd.Dir = root
	buildStdout := newSlogWriter(slog.LevelInfo, "make")
	buildStderr := newSlogWriter(slog.LevelWarn, "make")
	buildCmd.Stdout = buildStdout
	buildCmd.Stderr = buildStderr

	buildErr := buildCmd.Run()

	buildStdout.Flush()
	buildStderr.Flush()

	if buildErr != nil {
		return fmt.Errorf("make build: %w", buildErr)
	}

	watcherBin := filepath.Join(root, "bin", "watcher")
	workerBin := filepath.Join(root, "bin", "worker")

	// Write watcher YAML config.
	watcherCfg := filepath.Join(root, "bin", "e2e-watcher.yaml")
	cfgContent := fmt.Sprintf("watches:\n"+
		"  - name: radarr\n    path: %s\n    mediaType: movie\n    ignorePatterns:\n      - \\.tmp$\n"+
		"  - name: sonarr\n    path: %s\n    mediaType: show\n    ignorePatterns:\n      - \\.tmp$\n",
		filepath.Join(downloadsDir, "radarr"),
		filepath.Join(downloadsDir, "sonarr"),
	)

	if err := os.WriteFile(watcherCfg, []byte(cfgContent), 0o644); err != nil {
		return fmt.Errorf("write watcher config: %w", err)
	}

	baseEnv := append(os.Environ(),
		"HATCHET_CLIENT_TOKEN="+hatchetToken,
		"HATCHET_CLIENT_TLS_STRATEGY=none",
	)

	// Start watcher subprocess.
	watcherCmd = exec.Command(watcherBin, "--config", watcherCfg)
	watcherCmd.Env = baseEnv
	// These already use slog so write directly to stdout/stderr to avoid double encapsulation
	watcherCmd.Stdout = os.Stdout
	watcherCmd.Stderr = os.Stderr

	if err := watcherCmd.Start(); err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}

	log.Info("watcher started", "pid", watcherCmd.Process.Pid)

	// Start worker subprocess with path-translation env vars.
	workerEnv := append(baseEnv,
		"MEDIA_OUTPUT_DIR="+processedDir,
		"MEDIA_WATCHER_ROOT="+downloadsDir,
		"RADARR_URL="+radarrBase,
		"RADARR_API_KEY="+radarrAPIKey,
		"SONARR_URL="+sonarrBase,
		"SONARR_API_KEY="+sonarrAPIKey,
		"RADARR_LOCAL_PATH_PREFIX="+filepath.Join(downloadsDir, "radarr"),
		"RADARR_REMOTE_PATH_PREFIX=/downloads",
		"SONARR_LOCAL_PATH_PREFIX="+filepath.Join(downloadsDir, "sonarr"),
		"SONARR_REMOTE_PATH_PREFIX=/downloads",
	)

	workerCmd = exec.Command(workerBin)
	workerCmd.Env = workerEnv
	workerCmd.Stdout = os.Stdout
	workerCmd.Stderr = os.Stderr

	if err := workerCmd.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}

	log.Info("worker started", "pid", workerCmd.Process.Pid)

	return nil
}

func stopProcesses() {
	for _, cmd := range []*exec.Cmd{watcherCmd, workerCmd} {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
}

// moduleRoot returns the absolute path of the Go module root by invoking
// `go env GOMOD`.
func moduleRoot() string {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err == nil {
		mod := strings.TrimSpace(string(out))
		if mod != "" && mod != os.DevNull {
			return filepath.Dir(mod)
		}
	}

	// Fallback: test runs from e2e/, so module root is one level up.
	abs, _ := filepath.Abs("..")

	return abs
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
