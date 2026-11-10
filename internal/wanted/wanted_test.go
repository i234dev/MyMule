package wanted

import (
	"path/filepath"
	"testing"
	"time"
)

// The list is kept on disk; an entry is added once; the one waiting
// longest is searched for first, never one downloading.
func TestStore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wanted.json")
	s := Open(p)
	a, _ := s.Add(Entry{WorkID: "Q1", Kind: "movie", Title: "Una Película"})
	b, _ := s.Add(Entry{WorkID: "Q2", Kind: "series", Title: "Una Serie", Season: 1, Episode: 2})
	if again, _ := s.Add(Entry{WorkID: "Q1", Title: "otra vez"}); again.Title != "Una Película" || len(s.List()) != 2 {
		t.Fatal("added twice")
	}
	if b.ID != "Q2:s1e2" || a.ID != "Q1" {
		t.Fatalf("ids %q %q", a.ID, b.ID)
	}
	now := time.Now()
	s.Searched(a.ID, now.Add(-2*time.Hour))
	if e, ok := s.Due(now, time.Hour); !ok || e.ID != b.ID {
		t.Fatalf("due %+v, want the one never searched for", e)
	}
	s.Found(b.ID, "abcd", "file.mkv", now)
	if e, ok := s.Due(now, time.Hour); !ok || e.ID != a.ID {
		t.Fatalf("due %+v", e)
	}
	s.Searched(a.ID, now)
	if _, ok := s.Due(now, time.Hour); ok {
		t.Fatal("due again too soon")
	}
	s.SearchNow(a.ID)
	if e, ok := s.Due(now, time.Hour); !ok || e.ID != a.ID {
		t.Fatal("search now ignored")
	}

	re := Open(p)
	if e, ok := re.Get(b.ID); !ok || e.Status != Downloading || e.Hash != "ABCD" {
		t.Fatalf("not kept: %+v", e)
	}
	re.Completed("ABCD")
	if e, _ := re.Get(b.ID); e.Status != Done {
		t.Fatalf("not done: %+v", e)
	}
	if err := re.Remove(a.ID); err != nil || re.Remove(a.ID) != ErrNotFound {
		t.Fatal("remove")
	}
}

func TestStoreFull(t *testing.T) {
	s := Open("")
	for i := range MaxEntries {
		if _, err := s.Add(Entry{WorkID: MakeID("Q", i, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add(Entry{WorkID: "Q-last"}); err != ErrFull {
		t.Fatalf("over the limit: %v", err)
	}
}
