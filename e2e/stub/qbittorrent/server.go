//go:build e2e

// Package qbittorrent provides an in-process stub for the qBittorrent Web API v2.
// It is used by the e2e test suite to simulate a download client without needing
// a real qBittorrent instance.
package qbittorrent

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// logger is a slog.Logger tagged with source="qbt" so its messages are
// distinguishable from test-harness logs when output is interleaved.
var logger = slog.Default().With("source", "qbt") //nolint:gochecknoglobals

// Torrent holds the minimal torrent state that Radarr and Sonarr read from
// GET /api/v2/torrents/info.
type Torrent struct {
	Hash       string  `json:"hash"`
	Name       string  `json:"name"`
	State      string  `json:"state"`
	Category   string  `json:"category"`
	Progress   float64 `json:"progress"`
	Size       int64   `json:"size"`
	Downloaded int64   `json:"downloaded"`
	Eta        int     `json:"eta"`
}

// Category holds a qBittorrent download category.
// Radarr and Sonarr verify category creation by reading GET /api/v2/torrents/categories
// after POST /api/v2/torrents/createCategory; the category must appear in the response.
type Category struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

// Server is an in-process qBittorrent Web API stub.
// On torrent-add it copies a fixture file into the download directory so the
// watcher subprocess can detect it.
type Server struct {
	mu          sync.Mutex
	torrents    map[string]*Torrent
	categories  map[string]*Category
	listener    net.Listener
	server      *http.Server
	fixturePath string
	downloadDir string
}

// New creates and starts a new stub server bound to 0.0.0.0:0.
// fixturePath is the file copied to the download directory when a torrent is added.
// downloadDir is the root download directory whose sub-directories correspond to
// download client categories (e.g., /tmp/e2e-media-processor/downloads).
func New(fixturePath, downloadDir string) (*Server, error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	s := &Server{
		torrents:    make(map[string]*Torrent),
		categories:  make(map[string]*Category),
		listener:    ln,
		fixturePath: fixturePath,
		downloadDir: downloadDir,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/auth/login", s.handleLogin)
	mux.HandleFunc("GET /api/v2/app/version", s.handleVersion)
	mux.HandleFunc("GET /api/v2/app/webapiVersion", s.handleWebAPIVersion)
	mux.HandleFunc("GET /api/v2/app/preferences", s.handlePreferences)
	mux.HandleFunc("GET /api/v2/torrents/categories", s.handleTorrentsCategories)
	mux.HandleFunc("POST /api/v2/torrents/createCategory", s.handleTorrentsCreateCategory)
	mux.HandleFunc("GET /api/v2/torrents/info", s.handleTorrentsInfo)
	mux.HandleFunc("POST /api/v2/torrents/add", s.handleTorrentsAdd)
	mux.HandleFunc("POST /api/v2/torrents/delete", s.handleTorrentsDelete)
	// Catch-all: log unmatched requests and return empty JSON so connection tests pass.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("unmatched request", "method", r.Method, "path", r.URL.RequestURI(), "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, "{}")
	})

	s.server = &http.Server{Handler: mux}

	go func() { _ = s.server.Serve(ln) }()

	return s, nil
}

// Port returns the TCP port the stub is listening on.
func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Close shuts down the stub server.
func (s *Server) Close() {
	_ = s.server.Close()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	logger.Debug("login", "remote", r.RemoteAddr)

	_, _ = fmt.Fprint(w, "Ok.")
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	logger.Debug("version", "remote", r.RemoteAddr)

	_, _ = fmt.Fprint(w, "5.0.0")
}

func (s *Server) handleWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	logger.Debug("webapiVersion", "remote", r.RemoteAddr)

	_, _ = fmt.Fprint(w, "2.9.3")
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	logger.Debug("preferences", "remote", r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"save_path":"/downloads","use_subcategories":false,"use_category_paths_in_manual_mode":true,"dht":true}`)
}

func (s *Server) handleTorrentsCategories(w http.ResponseWriter, r *http.Request) {
	logger.Debug("torrents/categories", "remote", r.RemoteAddr)

	s.mu.Lock()
	out := make(map[string]*Category, len(s.categories))

	for name, category := range s.categories {
		out[name] = category
	}

	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleTorrentsCreateCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	name := r.FormValue("category")
	savePath := r.FormValue("savePath")

	logger.Debug("createCategory", "name", name, "savePath", savePath, "remote", r.RemoteAddr)

	if name == "" {
		http.Error(w, "category name required", http.StatusBadRequest)

		return
	}

	s.mu.Lock()
	s.categories[name] = &Category{Name: name, SavePath: savePath}
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTorrentsInfo(w http.ResponseWriter, r *http.Request) {
	categoryFilter := r.URL.Query().Get("category")

	s.mu.Lock()

	list := make([]*Torrent, 0, len(s.torrents))
	for _, torrent := range s.torrents {
		if categoryFilter == "" || torrent.Category == categoryFilter {
			list = append(list, torrent)
		}
	}

	s.mu.Unlock()

	logger.Debug("torrents/info", "query", r.URL.RawQuery, "count", len(list), "remote", r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleTorrentsAdd(w http.ResponseWriter, r *http.Request) {
	// Accept both multipart/form-data and application/x-www-form-urlencoded.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		_ = r.ParseForm()
	}

	category := r.FormValue("category")

	magnetStr := r.FormValue("urls")
	if magnetStr == "" {
		magnetStr = r.FormValue("magnet")
	}

	releaseName := extractDN(magnetStr)
	if releaseName == "" {
		releaseName = "unknown-release"
	}

	hash := extractBTIH(magnetStr)
	if hash == "" {
		hash = "0000000000000000000000000000000000000000"
	}

	logger.Debug("torrents/add", "category", category, "releaseName", releaseName, "hash", hash, "remote", r.RemoteAddr)

	destDir := filepath.Join(s.downloadDir, category, releaseName)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir %s: %v", destDir, err), http.StatusInternalServerError)

		return
	}

	destPath := filepath.Join(destDir, releaseName+".mp4")
	tmpPath := destPath + ".tmp"

	if err := copyFile(s.fixturePath, tmpPath); err != nil {
		http.Error(w, fmt.Sprintf("copy fixture: %v", err), http.StatusInternalServerError)

		return
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)

		http.Error(w, fmt.Sprintf("rename fixture: %v", err), http.StatusInternalServerError)

		return
	}

	var sz int64

	if info, err := os.Stat(destPath); err == nil {
		sz = info.Size()
	}

	s.mu.Lock()
	s.torrents[hash] = &Torrent{
		Hash:       hash,
		Name:       releaseName,
		State:      "uploading",
		Category:   category,
		Progress:   1.0,
		Size:       sz,
		Downloaded: sz,
		Eta:        0,
	}
	s.mu.Unlock()

	_, _ = fmt.Fprint(w, "Ok.")
}

func (s *Server) handleTorrentsDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	s.mu.Lock()
	for _, hash := range strings.Split(r.FormValue("hashes"), "|") {
		hash = strings.TrimSpace(hash)
		if hash == "" {
			continue
		}

		delete(s.torrents, hash)
	}
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

// extractDN parses the dn (display name) query parameter from a magnet URI.
func extractDN(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil || u.Scheme != "magnet" {
		return ""
	}

	return u.Query().Get("dn")
}

// extractBTIH parses the xt=urn:btih:<hash> parameter from a magnet URI and
// returns the hash in lowercase. Returns an empty string if not found.
func extractBTIH(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil || u.Scheme != "magnet" {
		return ""
	}

	for _, xt := range u.Query()["xt"] {
		if after, ok := strings.CutPrefix(xt, "urn:btih:"); ok {
			return strings.ToLower(after)
		}
	}

	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()

		return fmt.Errorf("copy: %w", err)
	}

	return out.Close()
}
