package prefs

import (
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "preferences.json")
	s := Open(path, base)
	if s.Get().Library.MoviesDir != "" || !s.Get().Library.Share {
		t.Fatalf("defaults: %+v", s.Get())
	}

	p := s.Get()
	p.Library.MoviesDir = "Movies" // relative to the data folder
	p.Library.SeriesDir = filepath.Join(t.TempDir(), "Series")
	if err := s.Set(p); err != nil {
		t.Fatal(err)
	}
	if got := Open(path, base).Library().MoviesDir; got != filepath.Join(base, "Movies") {
		t.Fatalf("movies dir after reload: %s", got)
	}

	bad := s.Get()
	bad.Library.SeriesTemplate = "{title} ({year})" // no episode
	if s.Set(bad) == nil {
		t.Fatal("bad template saved")
	}
	if s.Get().Library.SeriesTemplate == "{title} ({year})" {
		t.Fatal("bad template applied")
	}
}
