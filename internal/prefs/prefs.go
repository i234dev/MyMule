// Package prefs keeps the preferences edited from the web interface in
// preferences.json, next to mymule.conf.
package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"mymule/internal/fsutil"
	"mymule/internal/library"
)

// Preferences edited from the web interface.
type Preferences struct {
	Library library.Settings `json:"library"`
	Search  Search           `json:"search"`
	Server  string           `json:"server"` // preferred eD2K server (ip:port); empty = automatic
}

// Search says which files of a movie or series are preferred.
type Search struct {
	Languages  []string `json:"languages"`   // audio languages, best first; empty = the browser's
	Subs       bool     `json:"subs"`        // original version with subtitles is fine
	Resolution string   `json:"resolution"`  // 2160p, 1080p, 720p or 480p
	MaxSizeGB  float64  `json:"max_size_gb"` // 0 = no limit
	HideCam    bool     `json:"hide_cam"`    // hide cam/telesync/screener copies
}

// Defaults are the preferences before the user changes anything.
func Defaults() Preferences {
	return Preferences{Library: library.DefaultSettings(),
		Search: Search{Subs: true, Resolution: "1080p", HideCam: true}}
}

var (
	resolutions = map[string]bool{"2160p": true, "1080p": true, "720p": true, "480p": true}
	langCodeRe  = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,4})?$`)
)

// Store loads and saves preferences.json.
type Store struct {
	path    string
	base    string // folder relative paths are resolved against
	mu      sync.Mutex
	p       Preferences
	protect []string // folders a library folder must not overlap at all
	hosts   []string // computers whose network folders are accepted
}

// Open loads preferences from path (defaults if missing). Relative folder
// names are relative to base (the data folder).
func Open(path, base string) *Store {
	s := &Store{path: path, base: base, p: Defaults()}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s.p)
		// Saved before the "organize" switch existed: organising was on
		// whenever a folder was set, so keep it that way.
		var probe struct {
			Library map[string]json.RawMessage `json:"library"`
		}
		if json.Unmarshal(b, &probe) == nil && probe.Library != nil {
			if _, has := probe.Library["organize"]; !has {
				s.p.Library.Organize = s.p.Library.MoviesDir != "" || s.p.Library.SeriesDir != ""
			}
		}
	}
	if s.p.Search.Resolution == "" {
		s.p.Search = Defaults().Search
	}
	if s.p.Library.MovieTemplate == "" {
		s.p.Library.MovieTemplate = library.MoviePresets[0]
	}
	if s.p.Library.SeriesTemplate == "" {
		s.p.Library.SeriesTemplate = library.SeriesPresets[0]
	}
	return s
}

// Protect adds folders (such as the temporary folder) that a library
// folder may not be, contain, or be inside. The data folder may hold
// library folders but may not be one or be inside one.
func (s *Store) Protect(dirs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range dirs {
		if d != "" {
			if abs, err := filepath.Abs(d); err == nil {
				s.protect = append(s.protect, abs)
			}
		}
	}
}

// AllowNetworkHosts sets the computers whose Windows network folders may
// be library folders (network_hosts in mymule.conf).
func (s *Store) AllowNetworkHosts(hosts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts = append([]string(nil), hosts...)
}

// Get returns the current preferences.
func (s *Store) Get() Preferences {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p
}

// Abs resolves a folder from the preferences (relative to the data folder).
func (s *Store) Abs(dir string) string {
	if dir == "" || filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(s.base, dir)
}

// Library returns the library settings with absolute folders. A folder
// that is not acceptable (for instance saved by an older version) is
// returned empty, so nothing is shared from it or moved into it.
func (s *Store) Library() library.Settings {
	l := s.Get().Library
	l.MoviesDir, l.SeriesDir = s.usable(l.MoviesDir), s.usable(l.SeriesDir)
	return l
}

// check applies checkFolder with this store's protected folders.
func (s *Store) check(abs string) error {
	s.mu.Lock()
	noInside := append([]string{}, s.protect...)
	hosts := s.hosts
	s.mu.Unlock()
	base := s.base
	if b, err := filepath.Abs(base); err == nil {
		base = b
	}
	return checkFolder(abs, []string{base}, noInside, hosts)
}

// usable resolves a library folder, or returns "" if it is not acceptable.
func (s *Store) usable(dir string) string {
	abs := s.Abs(dir)
	if abs == "" {
		return ""
	}
	if s.check(abs) != nil {
		return ""
	}
	return abs
}

// Set validates and saves new preferences. Folders are created and must
// be writable.
func (s *Store) Set(p Preferences) error {
	l := &p.Library
	l.MoviesDir, l.SeriesDir = strings.TrimSpace(l.MoviesDir), strings.TrimSpace(l.SeriesDir)
	l.MovieTemplate, l.SeriesTemplate = strings.TrimSpace(l.MovieTemplate), strings.TrimSpace(l.SeriesTemplate)
	p.Server = strings.TrimSpace(p.Server)
	if p.Server != "" {
		if _, port, err := net.SplitHostPort(p.Server); err != nil || port == "" {
			return errors.New("the server must be ip:port, or empty for automatic")
		}
	}
	sp := &p.Search
	var langs []string
	for _, c := range sp.Languages {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if !langCodeRe.MatchString(c) {
			return fmt.Errorf("%q is not a language code (e.g. es, en, es-419)", c)
		}
		langs = append(langs, c)
	}
	if len(langs) > 8 {
		return errors.New("too many languages (8 at most)")
	}
	sp.Languages = langs
	if !resolutions[sp.Resolution] {
		return errors.New("resolution must be 2160p, 1080p, 720p or 480p")
	}
	if sp.MaxSizeGB < 0 || sp.MaxSizeGB > 1000 {
		return errors.New("the maximum size must be between 0 (no limit) and 1000 GB")
	}
	if err := library.CheckTemplate(l.MovieTemplate, false); err != nil {
		return fmt.Errorf("movie template: %w", err)
	}
	if err := library.CheckTemplate(l.SeriesTemplate, true); err != nil {
		return fmt.Errorf("series template: %w", err)
	}
	for _, d := range []string{l.MoviesDir, l.SeriesDir} {
		if d == "" {
			continue
		}
		if err := s.check(s.Abs(d)); err != nil {
			return fmt.Errorf("folder %s: %w", d, err)
		}
		if err := writable(s.Abs(d)); err != nil {
			return fmt.Errorf("folder %s: %w", d, err)
		}
		// Checked again now that it exists, in case it is a link.
		if err := s.check(s.Abs(d)); err != nil {
			return fmt.Errorf("folder %s: %w", d, err)
		}
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fsutil.WriteFile(s.path, b, 0o600); err != nil {
		return err
	}
	s.p = p
	return nil
}

// writable creates dir if needed and checks a file can be written in it.
// The error names the user MyMule runs as, since that is usually what to
// fix (a folder owned by another user, such as root on a NAS).
func writable(dir string) error {
	who := "MyMule"
	if u, err := user.Current(); err == nil && u.Username != "" {
		who = "MyMule (user " + u.Username + ")"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%s can't create it; create it and let that user write in it", who)
	}
	f, err := os.CreateTemp(dir, ".mymule-write-test-*")
	if err != nil {
		return fmt.Errorf("%s can't write in it; give that user permission to write there", who)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
