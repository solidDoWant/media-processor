//go:build e2e

// Package e2e_test contains end-to-end tests for the media-processor pipeline.
// It spins up Radarr, Sonarr, Temporal, the watcher, and the worker via Docker
// Compose, and verifies the full happy-path flow.
//
// Run with: make test-e2e
// Prerequisites: Docker, internet access (first run downloads the BBB fixture).
package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/solidDoWant/media-processor/e2e/stub/qbittorrent"
)

// Fixed base paths under which all e2e state lives.
const (
	baseDir      = "/tmp/e2e-media-processor"
	downloadsDir = "/tmp/e2e-media-processor/downloads"
	processedDir = "/tmp/e2e-media-processor/processed-output"
	configDir    = "/tmp/e2e-media-processor/config"

	radarrAPIKey = "e2e-radarr-api-key-e2e"
	sonarrAPIKey = "e2e-sonarr-api-key-e2e"

	radarrBase = "http://localhost:7878"
	sonarrBase = "http://localhost:8989"

	bbbZipURL  = "https://download.blender.org/demo/movies/BBB/bbb_sunflower_1080p_30fps_normal.mp4.zip"
	bbbMP4Name = "bbb_sunflower_1080p_30fps_normal.mp4"

	// Fixed host-side ports for the watcher and the three worker pools'
	// Prometheus endpoints, bound by the compose services. The compose file
	// runs three worker containers — one polling the workflow queue, one the
	// transcode activity queue, and one every other activity queue — so the
	// workflow's metrics fan out across them and the e2e tests have to
	// aggregate.
	watcherMetricsAddr         = "127.0.0.1:19090"
	workerWorkflowMetricsAddr  = "127.0.0.1:19091"
	workerTranscodeMetricsAddr = "127.0.0.1:19094"
	workerRestMetricsAddr      = "127.0.0.1:19096"

	// Fixed host-side ports for the watcher and worker pools' HTTP health
	// endpoints, bound by the compose services.
	watcherHealthBase         = "http://127.0.0.1:19092"
	workerWorkflowHealthBase  = "http://127.0.0.1:19093"
	workerTranscodeHealthBase = "http://127.0.0.1:19095"
	workerRestHealthBase      = "http://127.0.0.1:19097"

	// Host-mapped Temporal frontend port (compose binds 127.0.0.1:17233:7233).
	// A non-default host port avoids colliding with a developer's local
	// `make temporal-up` dev stack which binds host port 7233.
	temporalHostPort  = "127.0.0.1:17233"
	temporalNamespace = "default"
)

//nolint:gochecknoglobals // immutable side-by-side metadata for the worker pools.
var (
	// workerServiceNames lists the docker-compose service names for the three
	// worker pools. Used to bring them up, stream their logs, and route compose
	// commands.
	workerServiceNames = []string{"worker-workflow", "worker-transcode", "worker-rest"}

	// workerHealthBases lists every worker pool's /readyz base URL. Used by
	// the health monitor to confirm all three pools are ready before tests
	// proceed.
	workerHealthBases = []string{workerWorkflowHealthBase, workerTranscodeHealthBase, workerRestHealthBase}

	// workerMetricsAddrs lists every worker pool's Prometheus endpoint. Tests
	// fetch metrics from each and merge — see fetchAllWorkerMetrics.
	workerMetricsAddrs = []string{workerWorkflowMetricsAddr, workerTranscodeMetricsAddr, workerRestMetricsAddr}
)

// log is a package-level slog.Logger tagged with source="e2e" so test-harness
// messages are distinguishable from subprocess output in interleaved logs.
var log = slog.Default().With("source", "e2e") //nolint:gochecknoglobals

// Package-level state set during TestMain and shared across test functions.
var (
	radarrMovieID   int
	sonarrSeriesID  int
	sonarrEpisodeID int
)

func TestMain(m *testing.M) {
	// Tear down containers if the process is terminated early (e.g. SIGTERM
	// from a CI timeout or Ctrl-C) before the deferred composeDown in run()
	// has a chance to execute.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		log.Warn("received signal, tearing down containers", "signal", sig)
		composeDown()
		os.Exit(1)
	}()

	if err := run(m); err != nil {
		log.Error("e2e run failed", "error", err)
		os.Exit(1)
	}
}

func run(m *testing.M) error {
	// 0. Tear down any leftover services from a previous killed run.
	// This ensures we always start from a clean Docker state even when the
	// previous run's deferred composeDown did not execute (e.g. SIGKILL).
	composeDown()

	// 1. Clean and recreate all directories.
	if err := resetDirs(); err != nil {
		return fmt.Errorf("resetDirs: %w", err)
	}

	// 2. Download and cache the Big Buck Bunny fixture.
	// A 30-minute timeout ensures the suite fails fast on a stalled download
	// rather than hanging until the global test timeout fires.
	downloadCtx, downloadCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer downloadCancel()

	fixturePath, err := ensureBBBFixture(downloadCtx)
	if err != nil {
		return fmt.Errorf("ensureBBBFixture: %w", err)
	}

	// 3. Start the in-process qBittorrent stub.
	// It binds to 0.0.0.0 so Docker containers can reach it via host.docker.internal.
	qbtStub, err := qbittorrent.New(fixturePath, downloadsDir)
	if err != nil {
		return fmt.Errorf("start qbt stub: %w", err)
	}

	defer qbtStub.Close()

	log.Info("qBittorrent stub listening", "port", qbtStub.Port())

	// 4. Pull Docker images (may download several hundred MB on first run).
	pullCtx, pullCancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer pullCancel()

	if err = composePull(pullCtx); err != nil {
		return fmt.Errorf("compose pull: %w", err)
	}

	// 5. Docker Compose up (infrastructure: postgres, temporal, radarr, sonarr).
	if err = composeUp(); err != nil {
		composeDown()

		return fmt.Errorf("compose up: %w", err)
	}

	defer composeDown()

	// 6. Wait for Radarr, Sonarr, and Temporal to be healthy.
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Minute)

	if err = waitForServices(healthCtx); err != nil {
		healthCancel()

		return fmt.Errorf("waitForServices: %w", err)
	}

	healthCancel()

	// 7. Configure Radarr (root folder, quality profile, download client, movie).
	radarrMovieID, err = configureRadarr(context.Background(), qbtStub.Port())
	if err != nil {
		return fmt.Errorf("configureRadarr: %w", err)
	}

	log.Info("Radarr configured", "movieID", radarrMovieID)

	// 8. Configure Sonarr (root folder, quality profile, download client, series).
	sonarrSeriesID, err = configureSonarr(context.Background(), qbtStub.Port())
	if err != nil {
		return fmt.Errorf("configureSonarr: %w", err)
	}

	log.Info("Sonarr configured", "seriesID", sonarrSeriesID)

	// Fetch S01E01 episode ID for use in the Sonarr release push.
	sonarrEpisodeID, err = fetchSonarrS01E01(context.Background(), sonarrSeriesID)
	if err != nil {
		return fmt.Errorf("fetchSonarrS01E01: %w", err)
	}

	log.Info("Sonarr S01E01 fetched", "episodeID", sonarrEpisodeID)

	// 9. Start watcher and worker containers. Compose blocks until the
	// temporal-create-namespace bootstrap container has exited successfully.
	if err = composeUpWatcherWorker(context.Background()); err != nil {
		return fmt.Errorf("composeUpWatcherWorker: %w", err)
	}

	// 10. Stream watcher/worker logs to stdout and monitor health until both
	// services report /readyz before running any tests.
	monCtx, monCancel := context.WithCancel(context.Background())
	streamAppLogs(monCtx)
	readyCh, failCh := startHealthMonitor(monCtx)

	readyTimer := time.NewTimer(2 * time.Minute)
	select {
	case <-readyCh:
		readyTimer.Stop()
		log.Info("app services healthy")
	case <-readyTimer.C:
		monCancel()

		return fmt.Errorf("app services did not become healthy within 2 minutes")
	}

	code := m.Run()

	monCancel()

	// Propagate any health degradation observed during the test run.
	var healthErr error

	select {
	case healthErr = <-failCh:
		log.Error("app health degraded during test run", "error", healthErr)
	default:
	}

	if code != 0 {
		return errors.Join(fmt.Errorf("test suite failed (exit code %d)", code), healthErr)
	}

	return healthErr
}
