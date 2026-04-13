//go:build e2e

// Package qbittorrent provides an in-process stub for the qBittorrent Web API v2.
// It is used by the e2e test suite to simulate a download client without needing
// a real qBittorrent instance.
package qbittorrent

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

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
		log.Printf("qbt stub: unmatched request: %s %s (from %s)", r.Method, r.URL.RequestURI(), r.RemoteAddr)
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
	log.Printf("qbt stub: login from %s", r.RemoteAddr)

	_, _ = fmt.Fprint(w, "Ok.")
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	log.Printf("qbt stub: version from %s", r.RemoteAddr)

	_, _ = fmt.Fprint(w, "5.0.0")
}

func (s *Server) handleWebAPIVersion(w http.ResponseWriter, r *http.Request) {
	log.Printf("qbt stub: webapiVersion from %s", r.RemoteAddr)

	_, _ = fmt.Fprint(w, "2.9.3")
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	log.Printf("qbt stub: preferences from %s", r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"save_path":"/downloads","use_subcategories":false,"use_category_paths_in_manual_mode":true,"dht":true}`)
}

func (s *Server) handleTorrentsCategories(w http.ResponseWriter, r *http.Request) {
	log.Printf("qbt stub: torrents/categories from %s", r.RemoteAddr)

	s.mu.Lock()
	out := make(map[string]*Category, len(s.categories))

	for k, v := range s.categories {
		out[k] = v
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

	log.Printf("qbt stub: createCategory %q savePath=%q from %s", name, savePath, r.RemoteAddr)

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
	for _, t := range s.torrents {
		if categoryFilter == "" || t.Category == categoryFilter {
			list = append(list, t)
		}
	}

	s.mu.Unlock()

	log.Printf("qbt stub: torrents/info (query=%s) → %d torrents from %s", r.URL.RawQuery, len(list), r.RemoteAddr)

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

	hash := syntheticHash(releaseName)

	log.Printf("qbt stub: torrents/add category=%q releaseName=%q hash=%s from %s", category, releaseName, hash, r.RemoteAddr)

	destDir := filepath.Join(s.downloadDir, category)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir %s: %v", destDir, err), http.StatusInternalServerError)

		return
	}

	destPath := filepath.Join(destDir, releaseName+".mp4")

	if err := copyFile(s.fixturePath, destPath); err != nil {
		http.Error(w, fmt.Sprintf("copy fixture: %v", err), http.StatusInternalServerError)

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

	for _, h := range strings.Split(r.FormValue("hashes"), "|") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}

		s.mu.Lock()
		delete(s.torrents, h)
		s.mu.Unlock()
	}

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

// syntheticHash returns a deterministic 40-hex-char string derived from name.
// It is used as a stable fake infohash for the stub torrent registry.
func syntheticHash(name string) string {
	// FNV-1a 64-bit, zero-padded to 40 hex chars (20 bytes).
	var h uint64 = 14695981039346656037

	for _, b := range []byte(name) {
		h ^= uint64(b)
		h *= 1099511628211
	}

	return fmt.Sprintf("%040x", h)
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
