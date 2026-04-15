//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

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
	dcBody := qbtDownloadClientBody("e2e-radarr-qbt", qbtPort, "radarr", "category")

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

// configureSonarr adds the root folder, download client, and Colonel Bleep (TVDB 254376).
// Colonel Bleep (1957) is in the public domain (copyright lapsed without renewal in 1985)
// and has 6-minute episodes — short enough that the BBB fixture (9:56) passes Sonarr's
// EpisodeFileIsNotSampleSpecification (50% of episode runtime = 3 min).
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
	// Sonarr's qBittorrent integration uses "tvCategory" (not "category") as the
	// field name for the TV download category.
	dcBody := qbtDownloadClientBody("e2e-sonarr-qbt", qbtPort, "sonarr", "tvCategory")

	if err := c.post("/api/v3/downloadclient", dcBody, nil); err != nil {
		return 0, fmt.Errorf("add download client: %w", err)
	}

	// Look up Colonel Bleep by TVDB ID.
	var lookupResults []json.RawMessage

	if err := c.get("/api/v3/series/lookup?term=tvdb:254376", &lookupResults); err != nil {
		return 0, fmt.Errorf("lookup series tvdb:254376: %w", err)
	}

	if len(lookupResults) == 0 {
		return 0, fmt.Errorf("no results for tvdb:254376")
	}

	var lookupSeries map[string]any

	if err := json.Unmarshal(lookupResults[0], &lookupSeries); err != nil {
		return 0, fmt.Errorf("unmarshal lookup result: %w", err)
	}

	// Overlay mandatory add fields.
	lookupSeries["qualityProfileId"] = profileID
	lookupSeries["rootFolderPath"] = "/tv"
	lookupSeries["monitored"] = true
	lookupSeries["seasonFolder"] = true
	lookupSeries["addOptions"] = map[string]any{
		"searchForMissingEpisodes":     false,
		"searchForCutoffUnmetEpisodes": false,
		"monitor":                      "pilot",
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

	slog.Info("Sonarr series added", "title", addedSeries.Title, "id", addedSeries.ID)

	return addedSeries.ID, nil
}

// fetchSonarrS01E01 returns the Sonarr episode ID for Season 1, Episode 1 of the given series.
func fetchSonarrS01E01(seriesID int) (int, error) {
	c := newArrClient(sonarrBase, sonarrAPIKey)

	var episodes []struct {
		ID            int `json:"id"`
		SeasonNumber  int `json:"seasonNumber"`
		EpisodeNumber int `json:"episodeNumber"`
	}

	if err := c.get(fmt.Sprintf("/api/v3/episode?seriesId=%d&seasonNumber=1", seriesID), &episodes); err != nil {
		return 0, fmt.Errorf("fetch episodes for series %d: %w", seriesID, err)
	}

	for _, episode := range episodes {
		if episode.SeasonNumber == 1 && episode.EpisodeNumber == 1 {
			return episode.ID, nil
		}
	}

	return 0, fmt.Errorf("S01E01 not found for series %d (got %d episodes in season 1)", seriesID, len(episodes))
}

// qbtDownloadClientBody returns the JSON body for adding a qBittorrent download client
// to Radarr or Sonarr. categoryField is the Arr-service-specific field name that holds
// the qBittorrent category: Radarr uses "category"; Sonarr uses "tvCategory".
func qbtDownloadClientBody(name string, port int, category, categoryField string) map[string]any {
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
			{"name": categoryField, "value": category},
			{"name": "initialState", "value": 0},
			{"name": "sequentialOrder", "value": false},
			{"name": "firstAndLast", "value": false},
		},
	}
}
