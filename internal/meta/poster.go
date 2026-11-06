package meta

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"mymule/internal/fsutil"
)

// Posters serves TMDB poster images through MyMule, so the browser never
// contacts TMDB itself, and keeps them in a small disk cache.
type Posters struct {
	dir     string // cache folder
	max     int64  // cache size limit in bytes
	client  *http.Client
	BaseURL string // https://image.tmdb.org/t/p

	mu sync.Mutex
}

// NewPosters creates the poster proxy with a cache in dir of at most max bytes.
func NewPosters(dir string, max int64) *Posters {
	return &Posters{dir: dir, max: max, client: &http.Client{Timeout: 20 * time.Second},
		BaseURL: "https://image.tmdb.org/t/p"}
}

var (
	posterSizes = map[string]bool{"w92": true, "w154": true, "w185": true, "w342": true, "w500": true}
	posterFile  = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}\.(jpg|png|webp)$`)
)

const maxPosterBytes = 5 << 20

// ServeHTTP answers GET .../{size}/{file}, e.g. /web/poster/w185/abc.jpg.
func (p *Posters) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	size, file := r.PathValue("size"), r.PathValue("file")
	if !posterSizes[size] || !posterFile.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	ctype := "image/jpeg"
	switch filepath.Ext(file) {
	case ".png":
		ctype = "image/png"
	case ".webp":
		ctype = "image/webp"
	}
	cached := filepath.Join(p.dir, size+"_"+file)
	data, err := os.ReadFile(cached)
	if err != nil {
		data, err = p.fetch(r, size, file)
		if err != nil {
			http.Error(w, "poster not available", http.StatusBadGateway)
			return
		}
		p.save(cached, data)
	} else {
		now := time.Now()
		os.Chtimes(cached, now, now) // recently used: keep it longer
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=604800")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

func (p *Posters) fetch(r *http.Request, size, file string) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), "GET", p.BaseURL+"/"+size+"/"+file, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
		return nil, os.ErrNotExist
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPosterBytes+1))
	if err != nil || len(data) > maxPosterBytes {
		return nil, os.ErrInvalid
	}
	return data, nil
}

// save writes a poster to the cache and trims the cache to its limit,
// removing the least recently used files.
func (p *Posters) save(path string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if os.MkdirAll(p.dir, 0o700) != nil {
		return
	}
	if fsutil.WriteFile(path, data, 0o600) != nil {
		return
	}
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return
	}
	type f struct {
		path string
		size int64
		mod  time.Time
	}
	var files []f
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			files = append(files, f{filepath.Join(p.dir, e.Name()), info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	if total <= p.max {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, x := range files {
		if total <= p.max*8/10 {
			break
		}
		if os.Remove(x.path) == nil {
			total -= x.size
		}
	}
}
