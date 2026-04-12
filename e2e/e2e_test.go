//go:build e2e

// Package e2e_test contains end-to-end tests for the media-processor pipeline.
// It spins up Radarr, Sonarr, and Hatchet via Docker Compose, starts the
// watcher and worker subprocesses, and verifies the full happy-path flow.
//
// Run with: make test-e2e
// Prerequisites: Docker, internet access (first run downloads the BBB fixture).
package e2e_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/media-processor/e2e/stub/qbittorrent"
)

// Fixed base paths under which all e2e state lives.
const (
	baseDir      = "/tmp/e2e-media-processor"
	downloadsDir = "/tmp/e2e-media-processor/downloads"
	processedDir = "/tmp/e2e-media-processor/processed-output"
	configDir    = "/tmp/e2e-media-processor/config"

	radarrAPIKey = "e2e-radarr-api-key"
	sonarrAPIKey = "e2e-sonarr-api-key"

	radarrBase = "http://localhost:7878"
	sonarrBase = "http://localhost:8989"

	bbbZipURL  = "https://download.blender.org/demo/movies/BBB/bbb_sunflower_1080p_30fps_normal.mp4.zip"
	bbbMP4Name = "bbb_sunflower_1080p_30fps_normal.mp4"
)

// Package-level state set during TestMain.
var (
	hatchetToken   string
	qbtStub        *qbittorrent.Server
	radarrMovieID  int
	sonarrSeriesID int
	fixturePath    string // path to BBB mp4 fixture
	binDir         string // temp dir holding built binaries
	watcherCmd     *exec.Cmd
	workerCmd      *exec.Cmd
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	// 1. Clean and recreate all directories.
	if err := resetDirs(); err != nil {
		log.Printf("e2e: resetDirs: %v", err)

		return 1
	}

	// 2. Download and cache the Big Buck Bunny fixture.
	var err error

	fixturePath, err = ensureBBBFixture()
	if err != nil {
		log.Printf("e2e: ensureBBBFixture: %v", err)

		return 1
	}

	// 3. Start the in-process qBittorrent stub.
	// It binds to 0.0.0.0 so Docker containers can reach it via host.docker.internal.
	qbtStub, err = qbittorrent.New(fixturePath, downloadsDir)
	if err != nil {
		log.Printf("e2e: start qbt stub: %v", err)

		return 1
	}

	defer qbtStub.Close()

	log.Printf("e2e: qBittorrent stub listening on port %d", qbtStub.Port())

	// 4. Docker Compose up.
	if err = composeUp(); err != nil {
		log.Printf("e2e: compose up: %v", err)
		composeDown()

		return 1
	}

	defer composeDown()

	// 5. Wait for Radarr, Sonarr, and Hatchet to be healthy.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

	if err = waitForServices(ctx); err != nil {
		cancel()
		log.Printf("e2e: waitForServices: %v", err)

		return 1
	}

	cancel()

	// 6. Generate Hatchet client token.
	hatchetToken, err = generateHatchetToken()
	if err != nil {
		log.Printf("e2e: generateHatchetToken: %v", err)

		return 1
	}

	// 7. Configure Radarr (root folder, quality profile, download client, movie).
	radarrMovieID, err = configureRadarr(qbtStub.Port())
	if err != nil {
		log.Printf("e2e: configureRadarr: %v", err)

		return 1
	}

	log.Printf("e2e: Radarr movie ID %d", radarrMovieID)

	// 8. Configure Sonarr (root folder, quality profile, download client, series).
	sonarrSeriesID, err = configureSonarr(qbtStub.Port())
	if err != nil {
		log.Printf("e2e: configureSonarr: %v", err)

		return 1
	}

	log.Printf("e2e: Sonarr series ID %d", sonarrSeriesID)

	// 9. Build watcher and worker binaries, write watcher config, start both.
	if err = startProcesses(); err != nil {
		log.Printf("e2e: startProcesses: %v", err)
		stopProcesses()

		return 1
	}

	defer stopProcesses()

	return m.Run()
}

// ---- directory management -----------------------------------------------

func resetDirs() error {
	if err := os.RemoveAll(baseDir); err != nil {
		return fmt.Errorf("remove base dir: %w", err)
	}

	for _, d := range []string{
		filepath.Join(downloadsDir, "radarr"),
		filepath.Join(downloadsDir, "sonarr"),
		filepath.Join(processedDir, "radarr"),
		filepath.Join(processedDir, "radarr-library"),
		filepath.Join(processedDir, "sonarr"),
		filepath.Join(processedDir, "sonarr-library"),
		filepath.Join(configDir, "radarr"),
		filepath.Join(configDir, "sonarr"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	return nil
}

// ---- BBB fixture --------------------------------------------------------

func ensureBBBFixture() (string, error) {
	// testdata/cache/ is relative to the e2e package directory.
	cacheDir := "testdata/cache"

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache: %w", err)
	}

	mp4Path := filepath.Join(cacheDir, bbbMP4Name)

	if _, err := os.Stat(mp4Path); err == nil {
		log.Printf("e2e: BBB fixture already cached at %s", mp4Path)

		return mp4Path, nil
	}

	log.Printf("e2e: downloading BBB fixture from %s (this may take a while)...", bbbZipURL)

	zipPath := filepath.Join(cacheDir, bbbMP4Name+".zip")

	if err := downloadFile(bbbZipURL, zipPath); err != nil {
		return "", fmt.Errorf("download BBB zip: %w", err)
	}

	log.Printf("e2e: extracting BBB mp4 from zip...")

	if err := extractFromZip(zipPath, bbbMP4Name, mp4Path); err != nil {
		_ = os.Remove(zipPath)

		return "", fmt.Errorf("extract BBB mp4: %w", err)
	}

	// Remove the zip; only the mp4 needs to be cached.
	_ = os.Remove(zipPath)

	log.Printf("e2e: BBB fixture cached at %s", mp4Path)

	return mp4Path, nil
}

func downloadFile(rawURL, dest string) error {
	resp, err := http.Get(rawURL) //nolint:noctx // setup code, no request context needed
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	_, err = io.Copy(f, resp.Body)

	return err
}

// extractFromZip extracts the first file named targetName (basename match) from
// the zip at zipPath into destPath. It writes to a .tmp file first and renames
// on success to ensure the cached file is always complete.
func extractFromZip(zipPath, targetName, destPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}

	defer func() { _ = r.Close() }()

	for _, f := range r.File {
		if filepath.Base(f.Name) != targetName {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %s: %w", f.Name, err)
		}

		tmpPath := destPath + ".tmp"

		out, err := os.Create(tmpPath)
		if err != nil {
			_ = rc.Close()

			return err
		}

		if _, err = io.Copy(out, rc); err != nil {
			_ = rc.Close()
			_ = out.Close()
			_ = os.Remove(tmpPath)

			return fmt.Errorf("copy entry: %w", err)
		}

		_ = rc.Close()

		if err = out.Close(); err != nil {
			_ = os.Remove(tmpPath)

			return err
		}

		return os.Rename(tmpPath, destPath)
	}

	return fmt.Errorf("entry %q not found in zip", targetName)
}

// ---- Docker Compose -----------------------------------------------------

func composeArgs(subcmd ...string) []string {
	args := []string{"compose", "-p", "e2e-media-processor", "-f", "docker-compose.yml"}

	return append(args, subcmd...)
}

func composeUp() error {
	cmd := exec.Command("docker", composeArgs("up", "-d")...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func composeDown() {
	cmd := exec.Command("docker", composeArgs("down", "-v", "--remove-orphans")...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
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

	for _, s := range services {
		log.Printf("e2e: waiting for %s...", s.name)

		if err := pollUntil(ctx, 5*time.Second, s.fn); err != nil {
			return fmt.Errorf("%s not ready: %w", s.name, err)
		}

		log.Printf("e2e: %s ready", s.name)
	}

	return nil
}

func checkHTTP(url string) error {
	resp, err := http.Get(url) //nolint:noctx // health poll, no caller context
	if err != nil {
		return err
	}

	_ = resp.Body.Close()

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

// ---- Radarr / Sonarr configuration -------------------------------------

// arrClient is a minimal HTTP client for Radarr and Sonarr v3 APIs.
type arrClient struct {
	base       string
	apiKey     string
	httpClient *http.Client
}

func newArrClient(base, apiKey string) *arrClient {
	return &arrClient{base: base, apiKey: apiKey, httpClient: &http.Client{Timeout: 30 * time.Second}}
}

func (c *arrClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)

		return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(body))
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}

	return nil
}

func (c *arrClient) post(path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)

		return fmt.Errorf("POST %s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(respBody))
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}

	return nil
}

// configureRadarr adds the root folder, download client, and Big Buck Bunny (TMDB 10378).
// Returns the Radarr movie ID.
func configureRadarr(qbtPort int) (int, error) {
	c := newArrClient(radarrBase, radarrAPIKey)

	// Root folder.
	if err := c.post("/api/v3/rootfolder", map[string]any{"path": "/movies"}, nil); err != nil {
		return 0, fmt.Errorf("add root folder: %w", err)
	}

	// Quality profile — use the first available.
	var profiles []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}

	if err := c.get("/api/v3/qualityprofile", &profiles); err != nil {
		return 0, fmt.Errorf("get quality profiles: %w", err)
	}

	if len(profiles) == 0 {
		return 0, fmt.Errorf("no quality profiles found in Radarr")
	}

	profileID := profiles[0].ID

	// Download client (qBittorrent stub).
	dcBody := qbtDownloadClientBody("e2e-radarr-qbt", qbtPort, "radarr")

	if err := c.post("/api/v3/downloadclient", dcBody, nil); err != nil {
		return 0, fmt.Errorf("add download client: %w", err)
	}

	// Look up Big Buck Bunny by TMDB ID.
	var lookupResults []json.RawMessage

	if err := c.get("/api/v3/movie/lookup?term=tmdb:10378", &lookupResults); err != nil {
		return 0, fmt.Errorf("lookup movie tmdb:10378: %w", err)
	}

	if len(lookupResults) == 0 {
		return 0, fmt.Errorf("no results for tmdb:10378")
	}

	var lookupMovie map[string]any

	if err := json.Unmarshal(lookupResults[0], &lookupMovie); err != nil {
		return 0, fmt.Errorf("unmarshal lookup result: %w", err)
	}

	// Overlay mandatory add fields.
	lookupMovie["qualityProfileId"] = profileID
	lookupMovie["rootFolderPath"] = "/movies"
	lookupMovie["monitored"] = true
	lookupMovie["addOptions"] = map[string]any{"searchForMovie": false}

	var addedMovie struct {
		ID int `json:"id"`
	}

	if err := c.post("/api/v3/movie", lookupMovie, &addedMovie); err != nil {
		return 0, fmt.Errorf("add movie: %w", err)
	}

	if addedMovie.ID == 0 {
		return 0, fmt.Errorf("Radarr returned movie ID 0")
	}

	return addedMovie.ID, nil
}

// configureSonarr adds the root folder, download client, and The Lone Ranger (TVDB 72059).
// Returns the Sonarr series ID.
func configureSonarr(qbtPort int) (int, error) {
	c := newArrClient(sonarrBase, sonarrAPIKey)

	// Root folder.
	if err := c.post("/api/v3/rootfolder", map[string]any{"path": "/tv"}, nil); err != nil {
		return 0, fmt.Errorf("add root folder: %w", err)
	}

	// Quality profile — use the first available.
	var profiles []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}

	if err := c.get("/api/v3/qualityprofile", &profiles); err != nil {
		return 0, fmt.Errorf("get quality profiles: %w", err)
	}

	if len(profiles) == 0 {
		return 0, fmt.Errorf("no quality profiles found in Sonarr")
	}

	profileID := profiles[0].ID

	// Download client (qBittorrent stub).
	dcBody := qbtDownloadClientBody("e2e-sonarr-qbt", qbtPort, "sonarr")

	if err := c.post("/api/v3/downloadclient", dcBody, nil); err != nil {
		return 0, fmt.Errorf("add download client: %w", err)
	}

	// Look up The Lone Ranger by TVDB ID.
	var lookupResults []json.RawMessage

	if err := c.get("/api/v3/series/lookup?term=tvdb:72059", &lookupResults); err != nil {
		return 0, fmt.Errorf("lookup series tvdb:72059: %w", err)
	}

	if len(lookupResults) == 0 {
		return 0, fmt.Errorf("no results for tvdb:72059")
	}

	var lookupSeries map[string]any

	if err := json.Unmarshal(lookupResults[0], &lookupSeries); err != nil {
		return 0, fmt.Errorf("unmarshal lookup result: %w", err)
	}

	// Overlay mandatory add fields.
	lookupSeries["qualityProfileId"] = profileID
	lookupSeries["rootFolderPath"] = "/tv"
	lookupSeries["monitored"] = false
	lookupSeries["seasonFolder"] = true
	lookupSeries["addOptions"] = map[string]any{
		"searchForMissingEpisodes":     false,
		"searchForCutoffUnmetEpisodes": false,
		"monitor":                      "none",
	}

	var addedSeries struct {
		ID    int    `json:"id"`
		Title string `json:"title"`
	}

	if err := c.post("/api/v3/series", lookupSeries, &addedSeries); err != nil {
		return 0, fmt.Errorf("add series: %w", err)
	}

	if addedSeries.ID == 0 {
		return 0, fmt.Errorf("Sonarr returned series ID 0")
	}

	log.Printf("e2e: added Sonarr series %q (ID %d)", addedSeries.Title, addedSeries.ID)

	return addedSeries.ID, nil
}

// qbtDownloadClientBody returns the JSON body for adding a qBittorrent download client
// to Radarr or Sonarr. category should be "radarr" or "sonarr".
func qbtDownloadClientBody(name string, port int, category string) map[string]any {
	return map[string]any{
		"name":           name,
		"enable":         true,
		"protocol":       "torrent",
		"priority":       1,
		"implementation": "QBittorrent",
		"configContract": "QBittorrentSettings",
		"fields": []map[string]any{
			{"name": "host", "value": "host.docker.internal"},
			{"name": "port", "value": port},
			{"name": "useSsl", "value": false},
			{"name": "urlBase", "value": ""},
			{"name": "username", "value": ""},
			{"name": "password", "value": ""},
			{"name": "category", "value": category},
			{"name": "initialState", "value": 0},
			{"name": "sequentialOrder", "value": false},
			{"name": "firstAndLast", "value": false},
		},
	}
}

// ---- watcher + worker subprocesses --------------------------------------

func startProcesses() error {
	var err error

	binDir, err = os.MkdirTemp("", "e2e-bins-*")
	if err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}

	root := moduleRoot()

	// Build watcher.
	watcherBin := filepath.Join(binDir, "watcher")

	if out, err := buildBinary(root, "./cmd/watcher", watcherBin); err != nil {
		return fmt.Errorf("build watcher: %w\n%s", err, out)
	}

	// Build worker.
	workerBin := filepath.Join(binDir, "worker")

	if out, err := buildBinary(root, "./cmd/worker", workerBin); err != nil {
		return fmt.Errorf("build worker: %w\n%s", err, out)
	}

	// Write watcher YAML config.
	watcherCfg := filepath.Join(binDir, "watcher.yaml")
	cfgContent := fmt.Sprintf("watches:\n"+
		"  - name: radarr\n    path: %s\n    media_type: movie\n"+
		"  - name: sonarr\n    path: %s\n    media_type: show\n",
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
	watcherCmd.Stdout = os.Stdout
	watcherCmd.Stderr = os.Stderr

	if err := watcherCmd.Start(); err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}

	log.Printf("e2e: watcher started (PID %d)", watcherCmd.Process.Pid)

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

	log.Printf("e2e: worker started (PID %d)", workerCmd.Process.Pid)

	return nil
}

func stopProcesses() {
	for _, cmd := range []*exec.Cmd{watcherCmd, workerCmd} {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}

	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
}

func buildBinary(moduleRoot, pkg, out string) ([]byte, error) {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = moduleRoot

	return cmd.CombinedOutput()
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
// ctx is done. It calls cond immediately on the first iteration.
func pollUntil(ctx context.Context, interval time.Duration, cond func() error) error {
	for {
		if err := cond(); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ---- Tests --------------------------------------------------------------

// TestRadarrHappyPath pushes a Big Buck Bunny release to Radarr, waits for the
// pipeline to transcode it and notify Radarr, then verifies the movie is
// imported and the original .mp4 source file has been deleted.
func TestRadarrHappyPath(t *testing.T) {
	radarr := newArrClient(radarrBase, radarrAPIKey)

	const releaseTitle = "Big.Buck.Bunny.2008.1080p.WEB-DL"

	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%040x&dn=%s", 1, releaseTitle)

	require.NoError(t, radarr.post("/api/v3/release/push", map[string]any{
		"title":       releaseTitle,
		"downloadUrl": magnet,
		"protocol":    "Torrent",
		"publishDate": time.Now().UTC().Format(time.RFC3339),
		"indexer":     "e2e-test",
		"size":        700_000_000,
	}, nil), "push release to Radarr")

	// Poll until Radarr has imported the movie (hasFile=true).
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	err := pollUntil(ctx, 10*time.Second, func() error {
		var movie struct {
			HasFile bool `json:"hasFile"`
		}

		if err := radarr.get(fmt.Sprintf("/api/v3/movie/%d", radarrMovieID), &movie); err != nil {
			return err
		}

		if !movie.HasFile {
			return fmt.Errorf("movie not yet imported")
		}

		return nil
	})
	require.NoError(t, err, "Radarr did not import the movie within the timeout")

	// Source .mp4 must be deleted by the worker's cleanup step.
	sourceMp4 := filepath.Join(downloadsDir, "radarr", releaseTitle+".mp4")
	_, statErr := os.Stat(sourceMp4)

	assert.True(t, os.IsNotExist(statErr), "source .mp4 should have been deleted after import")

	// .mkv must exist somewhere under the Radarr library directory.
	foundMKV := findMKV(t, filepath.Join(processedDir, "radarr-library"))

	assert.True(t, foundMKV, "expected .mkv in radarr-library after import")
}

// TestSonarrHappyPath pushes an S01E01 release to Sonarr, waits for the
// pipeline to transcode it and notify Sonarr, then verifies an episode file
// was imported and the original .mp4 source file has been deleted.
func TestSonarrHappyPath(t *testing.T) {
	sonarr := newArrClient(sonarrBase, sonarrAPIKey)

	const releaseTitle = "The.Lone.Ranger.S01E01.1080p.WEB-DL"

	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%040x&dn=%s", 2, releaseTitle)

	require.NoError(t, sonarr.post("/api/v3/release/push", map[string]any{
		"title":       releaseTitle,
		"downloadUrl": magnet,
		"protocol":    "Torrent",
		"publishDate": time.Now().UTC().Format(time.RFC3339),
		"indexer":     "e2e-test",
		"size":        700_000_000,
	}, nil), "push release to Sonarr")

	// Poll until Sonarr has at least one imported episode file.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	err := pollUntil(ctx, 10*time.Second, func() error {
		var files []struct {
			ID int `json:"id"`
		}

		if err := sonarr.get(fmt.Sprintf("/api/v3/episodefile?seriesId=%d", sonarrSeriesID), &files); err != nil {
			return err
		}

		if len(files) == 0 {
			return fmt.Errorf("no episode files imported yet")
		}

		return nil
	})
	require.NoError(t, err, "Sonarr did not import any episode file within the timeout")

	// Source .mp4 must be deleted by the worker's cleanup step.
	sourceMp4 := filepath.Join(downloadsDir, "sonarr", releaseTitle+".mp4")
	_, statErr := os.Stat(sourceMp4)

	assert.True(t, os.IsNotExist(statErr), "source .mp4 should have been deleted after import")

	// .mkv must exist somewhere under the Sonarr library directory.
	foundMKV := findMKV(t, filepath.Join(processedDir, "sonarr-library"))

	assert.True(t, foundMKV, "expected .mkv in sonarr-library after import")
}

// findMKV returns true if any .mkv file exists under dir.
func findMKV(t *testing.T, dir string) bool {
	t.Helper()

	var found bool

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".mkv") {
			found = true
		}

		return nil
	})

	require.NoError(t, err)

	return found
}
