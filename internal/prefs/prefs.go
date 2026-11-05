// Package prefs keeps the preferences edited from the web interface in
// preferences.json, next to mymule.conf.
package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mymule/internal/library"
)

// Preferences edited from the web interface.
type Preferences struct {
	Library library.Settings `json:"library"`
}

// Defaults are the preferences before the user changes anything.
func Defaults() Preferences {
	return Preferences{Library: library.DefaultSettings()}
}

// Store loads and saves preferences.json.
type Store struct {
	path string
	base string // folder relative paths are resolved against
	mu   sync.Mutex
	p    Preferences
}

// Open loads preferences from path (defaults if missing). Relative folder
// names are relative to base (the data folder).
func Open(path, base string) *Store {
	s := &Store{path: path, base: base, p: Defaults()}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s.p)
	}
	if s.p.Library.MovieTemplate == "" {
		s.p.Library.MovieTemplate = library.MoviePresets[0]
	}
	if s.p.Library.SeriesTemplate == "" {
		s.p.Library.SeriesTemplate = library.SeriesPresets[0]
	}
	return s
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

// Library returns the library settings with absolute folders.
func (s *Store) Library() library.Settings {
	l := s.Get().Library
	l.MoviesDir, l.SeriesDir = s.Abs(l.MoviesDir), s.Abs(l.SeriesDir)
	return l
}

// Set validates and saves new preferences. Folders are created and must
// be writable.
func (s *Store) Set(p Preferences) error {
	l := &p.Library
	l.MoviesDir, l.SeriesDir = strings.TrimSpace(l.MoviesDir), strings.TrimSpace(l.SeriesDir)
	l.MovieTemplate, l.SeriesTemplate = strings.TrimSpace(l.MovieTemplate), strings.TrimSpace(l.SeriesTemplate)
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
		if err := writable(s.Abs(d)); err != nil {
			return fmt.Errorf("folder %s: %w", d, err)
		}
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.p = p
	return nil
}

// writable creates dir if needed and checks a file can be written in it.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.New("can't create it")
	}
	f, err := os.CreateTemp(dir, ".mymule-write-test-*")
	if err != nil {
		return errors.New("can't write in it")
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
