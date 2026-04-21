//go:build e2e

// Package e2e_test contains end-to-end tests for the media-processor pipeline.
// It spins up Radarr, Sonarr, Hatchet, the watcher, and the worker via Docker
// Compose, and verifies the full happy-path flow.
//
// Run with: make test-e2e
// Prerequisites: Docker, internet access (first run downloads the BBB fixture).
package e2e_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

	// Fixed host-side ports for the watcher and worker Prometheus endpoints,
	// bound by the compose services (127.0.0.1:19090 and 127.0.0.1:19091).
	watcherMetricsAddr = "127.0.0.1:19090"
	workerMetricsAddr  = "127.0.0.1:19091"
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

	// 5. Docker Compose up (infrastructure: postgres, hatchet, radarr, sonarr).
	if err = composeUp(); err != nil {
		composeDown()

		return fmt.Errorf("compose up: %w", err)
	}

	defer composeDown()

	// 6. Wait for Radarr, Sonarr, and Hatchet to be healthy.
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Minute)

	if err = waitForServices(healthCtx); err != nil {
		healthCancel()

		return fmt.Errorf("waitForServices: %w", err)
	}

	healthCancel()

	// 7. Generate Hatchet client token.
	hatchetToken, err := generateHatchetToken()
	if err != nil {
		return fmt.Errorf("generateHatchetToken: %w", err)
	}

	// 8. Configure Radarr (root folder, quality profile, download client, movie).
	radarrMovieID, err = configureRadarr(context.Background(), qbtStub.Port())
	if err != nil {
		return fmt.Errorf("configureRadarr: %w", err)
	}

	log.Info("Radarr configured", "movieID", radarrMovieID)

	// 9. Configure Sonarr (root folder, quality profile, download client, series).
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

	// 10. Start watcher and worker containers with the generated Hatchet token.
	if err = composeUpWatcherWorker(hatchetToken); err != nil {
		return fmt.Errorf("composeUpWatcherWorker: %w", err)
	}

	if code := m.Run(); code != 0 {
		return fmt.Errorf("test suite failed (exit code %d)", code)
	}

	return nil
}
