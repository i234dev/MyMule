package arr

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"mymule/internal/api"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/server"
)

// Config configures the adapter.
type Config struct {
	Manager     *download.Manager
	REST        *api.REST // searches and API key checks
	Key         string    // the API key (qBittorrent password)
	DownloadDir string    // where finished files end up
	StorePath   string    // small JSON file with categories (arr.json)
	Log         func(format string, args ...any)
}

// Adapter serves the Torznab indexer and the qBittorrent-compatible API.
type Adapter struct {
	cfg Config

	mu       sync.Mutex
	store    storeData
	sessions map[string]time.Time // qBittorrent login cookies
	hashes   map[ed2k.Hash]string // ed2k hash -> info hash (cache)

	// search runs eD2K searches; REST.Search by default, replaceable in tests.
	search func(ctx context.Context, q server.Query, global bool) ([]api.SearchResult, error)
}

type storeEntry struct {
	Category  string `json:"category,omitempty"`
	Added     int64  `json:"added,omitempty"`
	Completed int64  `json:"completed,omitempty"`
}

type storeData struct {
	Downloads  map[string]*storeEntry `json:"downloads"`  // by ed2k hash
	Categories map[string]string      `json:"categories"` // name -> save path
}

// New creates the adapter, loading its state file if present.
func New(cfg Config) *Adapter {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	a := &Adapter{cfg: cfg, sessions: map[string]time.Time{}, hashes: map[ed2k.Hash]string{}}
	if cfg.REST != nil {
		a.search = cfg.REST.Search
	}
	if data, err := os.ReadFile(cfg.StorePath); err == nil {
		json.Unmarshal(data, &a.store)
	}
	if a.store.Downloads == nil {
		a.store.Downloads = map[string]*storeEntry{}
	}
	if a.store.Categories == nil {
		a.store.Categories = map[string]string{}
	}
	return a
}

// Routes mounts the adapter's endpoints.
func (a *Adapter) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/torznab/api", a.torznab)
	mux.HandleFunc("/torznab/download/{hash}", a.torrentFile)
	mux.Handle("/api/v2/", a.qbit())
}

// saveLocked writes the state file (best effort).
func (a *Adapter) saveLocked() {
	if a.cfg.StorePath == "" {
		return
	}
	data, err := json.MarshalIndent(a.store, "", " ")
	if err != nil {
		return
	}
	tmp := a.cfg.StorePath + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, a.cfg.StorePath)
	}
}

// infoHash returns (and caches) the info hash of a download.
func (a *Adapter) infoHashLocked(f download.FileStatus) string {
	if h, ok := a.hashes[f.Hash]; ok {
		return h
	}
	h := InfoHash(ed2k.Link{Name: f.Name, Size: f.Size, Hash: f.Hash})
	a.hashes[f.Hash] = h
	return h
}

// findByInfoHash maps qBittorrent "hashes" (info hashes, separated by
// '|', or "all") to downloads.
func (a *Adapter) findByInfoHash(list string) []download.FileStatus {
	_, files := a.cfg.Manager.Status()
	if list == "all" {
		return files
	}
	want := map[string]bool{}
	for _, h := range strings.Split(strings.ToLower(list), "|") {
		if h = strings.TrimSpace(h); h != "" {
			want[h] = true
		}
	}
	var out []download.FileStatus
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range files {
		if want[a.infoHashLocked(f)] {
			out = append(out, f)
		}
	}
	return out
}
