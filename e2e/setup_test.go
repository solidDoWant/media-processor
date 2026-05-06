//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// ---- directory management -----------------------------------------------

func resetDirs() error {
	for _, dir := range []string{
		filepath.Join(downloadsDir, "radarr"),
		filepath.Join(downloadsDir, "sonarr"),
		filepath.Join(downloadsDir, "preserve-source"),
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

// composeUpWatcherWorker starts the watcher and the three worker pools
// (profile "app"). Compose blocks until temporal-create-namespace exits
// successfully (their depends_on condition), so the Temporal namespace is
// guaranteed to be registered before any app container starts.
func composeUpWatcherWorker(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	services := []string{"watcher"}
	for _, pool := range workerPools {
		services = append(services, pool.serviceName)
	}

	args := composeArgs(append([]string{"--profile", "app", "up", "-d"}, services...)...)

	cmd := exec.CommandContext(ctx, "docker", args...)
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

// streamAppLogs starts a goroutine that follows the watcher and every worker
// pool's logs and pipes the output directly to stdout. The goroutine exits
// when ctx is cancelled. Output lines are already prefixed with the service
// name by Docker Compose (e.g. "worker-transcode-1  | ...").
func streamAppLogs(ctx context.Context) {
	go func() {
		services := []string{"watcher"}
		for _, pool := range workerPools {
			services = append(services, pool.serviceName)
		}

		args := composeArgs(append([]string{"logs", "--follow"}, services...)...)

		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Env = composeEnv()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			log.Warn("app log streaming ended unexpectedly", "error", err)
		}
	}()
}

// startHealthMonitor polls the watcher and worker /readyz endpoints every 3s.
// The returned readyCh is closed once both services are seen healthy in the
// same poll. After ready, only the watcher is monitored for ongoing health —
// the worker pools are expected to drain themselves via WORKER_IDLE_EXIT_AFTER
// once the suite stops dispatching work, so a worker /readyz failure post-ready
// is normal and must not be reported as a degradation. The returned failCh
// receives at most one error if the watcher health subsequently degrades.
// Cancel ctx to stop the goroutine.
func startHealthMonitor(ctx context.Context) (readyCh <-chan struct{}, failCh <-chan error) {
	ready := make(chan struct{})
	fail := make(chan error, 1)

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		everReady := false

		poll := func() {
			watcherErr := checkHTTP(watcherHealthBase + "/readyz")

			if !everReady {
				workerErrs := make(map[string]error, len(workerPools))
				anyWorkerErr := false

				for _, pool := range workerPools {
					if err := checkHTTP(pool.healthBase + "/readyz"); err != nil {
						workerErrs[pool.serviceName] = err
						anyWorkerErr = true
					}
				}

				if watcherErr == nil && !anyWorkerErr {
					everReady = true

					close(ready)
				}

				return
			}

			if watcherErr == nil {
				return
			}

			// Watcher degraded after the initial ready signal — report once.
			select {
			case fail <- fmt.Errorf("app health degraded: watcher: %s", watcherErr.Error()):
			default:
			}
		}

		poll() // check immediately before the first tick

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				poll()
			}
		}
	}()

	return ready, fail
}

// waitForServices polls until Radarr, Sonarr, and Temporal are reachable.
// The Temporal check both dials the frontend and confirms that the configured
// namespace has been registered, which proves the temporal-create-namespace
// bootstrap container has completed successfully.
func waitForServices(ctx context.Context) error {
	type svc struct {
		name string
		fn   func() error
	}

	services := []svc{
		{"radarr", func() error { return checkHTTP(radarrBase + "/ping") }},
		{"sonarr", func() error { return checkHTTP(sonarrBase + "/ping") }},
		{"temporal", func() error { return checkTemporal(ctx) }},
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

// ---- Temporal readiness -------------------------------------------------

// checkTemporal dials the Temporal frontend on the host-mapped port and
// confirms the default namespace is registered. A successful DescribeNamespace
// proves both that the gRPC service is reachable AND that the bootstrap
// container has registered the namespace.
func checkTemporal(ctx context.Context) error {
	c, err := client.Dial(client.Options{
		HostPort:  temporalHostPort,
		Namespace: temporalNamespace,
	})
	if err != nil {
		return fmt.Errorf("dial temporal: %w", err)
	}
	defer c.Close()

	describeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if _, err := c.WorkflowService().DescribeNamespace(describeCtx, &workflowservice.DescribeNamespaceRequest{
		Namespace: temporalNamespace,
	}); err != nil {
		return fmt.Errorf("describe namespace %q: %w", temporalNamespace, err)
	}

	return nil
}

// ---- worker idle-exit verification --------------------------------------

// containerInspectState mirrors the subset of `docker inspect` State output
// the suite cares about. Only Status and ExitCode are read; the rest of the
// fields are deliberately ignored.
type containerInspectState struct {
	State struct {
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
}

// inspectWorkerState looks up the container ID of the named compose service
// and returns its parsed State. Returns an error wrapping "no container" when
// compose has no record of the service (e.g. the container never started).
func inspectWorkerState(ctx context.Context, serviceName string) (containerInspectState, error) {
	psCmd := exec.CommandContext(ctx, "docker", composeArgs("ps", "-aq", serviceName)...)
	psCmd.Env = composeEnv()

	out, err := psCmd.Output()
	if err != nil {
		return containerInspectState{}, fmt.Errorf("compose ps %s: %w", serviceName, err)
	}

	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return containerInspectState{}, fmt.Errorf("no container for service %q", serviceName)
	}

	inspectCmd := exec.CommandContext(ctx, "docker", "inspect", containerID)

	raw, err := inspectCmd.Output()
	if err != nil {
		return containerInspectState{}, fmt.Errorf("docker inspect %s: %w", containerID, err)
	}

	var inspected []containerInspectState
	if err := json.Unmarshal(raw, &inspected); err != nil {
		return containerInspectState{}, fmt.Errorf("parse docker inspect output for %s: %w", containerID, err)
	}

	if len(inspected) == 0 {
		return containerInspectState{}, fmt.Errorf("docker inspect returned no entries for %s", containerID)
	}

	return inspected[0], nil
}

// waitForWorkersExited polls every worker pool's compose service until each
// container has reached "exited" status with code 0, or the deadline elapses.
// Called after m.Run() to verify the WORKER_IDLE_EXIT_AFTER drain path runs
// to completion when no more work is dispatched. The deadline (2 min) covers
// the worst-case 5s idle window plus the 30s METRICS_SCRAPE_WAIT_TIMEOUT plus
// a generous buffer for staggered worker idle times. Returns an error naming
// any pool that did not exit cleanly within the deadline.
func waitForWorkersExited(ctx context.Context) error {
	deadline := 2 * time.Minute

	pollCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	return pollUntil(pollCtx, 2*time.Second, func() error {
		var unfinished []string

		for _, pool := range workerPools {
			state, err := inspectWorkerState(pollCtx, pool.serviceName)
			if err != nil {
				unfinished = append(unfinished, fmt.Sprintf("%s: %v", pool.serviceName, err))

				continue
			}

			if state.State.Status != "exited" {
				unfinished = append(unfinished, fmt.Sprintf("%s: status=%s", pool.serviceName, state.State.Status))

				continue
			}

			if state.State.ExitCode != 0 {
				return fmt.Errorf("worker %s exited with non-zero code %d", pool.serviceName, state.State.ExitCode)
			}
		}

		if len(unfinished) > 0 {
			return fmt.Errorf("workers not yet exited: %s", strings.Join(unfinished, "; "))
		}

		return nil
	})
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
