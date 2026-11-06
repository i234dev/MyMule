package prefs

import (
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	awayFromAppData(t)
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

	sp := s.Get()
	sp.Search.Languages = []string{" ES ", "en", ""}
	if err := s.Set(sp); err != nil || len(s.Get().Search.Languages) != 2 || s.Get().Search.Languages[0] != "es" {
		t.Fatalf("languages: %v %+v", err, s.Get().Search)
	}
	for _, f := range []func(*Preferences){
		func(p *Preferences) { p.Search.Languages = []string{"spanish"} },
		func(p *Preferences) { p.Search.Resolution = "8k" },
		func(p *Preferences) { p.Search.MaxSizeGB = -1 },
	} {
		x := s.Get()
		f(&x)
		if s.Set(x) == nil {
			t.Errorf("bad search preferences saved: %+v", x.Search)
		}
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
