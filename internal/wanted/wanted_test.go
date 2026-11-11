package wanted

import (
	"os"
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

// A whole season saved by an older version becomes one that gets every
// episode: the file it took counts for its episode. One that got the whole
// season in one file stays as it was.
func TestOldSeasonEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wanted.json")
	old := `[
 {"id":"tmdb-tv-1:s3e0","work_id":"tmdb-tv-1","kind":"series","title":"The Show","season":3,"lang":"en",
  "status":"done","hash":"A1A1456789ABCDEF0123456789ABCDEF","file":"The.Show.3x02.1080p.mkv"},
 {"id":"tmdb-tv-2:s1e0","work_id":"tmdb-tv-2","kind":"series","title":"Other Show","season":1,"lang":"en",
  "status":"done","hash":"B1B1456789ABCDEF0123456789ABCDEF","file":"Other.Show.S01.Complete.mkv"},
 {"id":"tmdb-tv-3:s2e0","work_id":"tmdb-tv-3","kind":"series","title":"Third Show","season":2,"lang":"en","status":"wanted"}
]`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(path)
	e, _ := s.Get("tmdb-tv-1:s3e0")
	if !e.Follow || e.FromSeason != 3 || e.ToSeason != 3 || e.Status != Following || len(e.Episodes) != 1 ||
		e.Episodes[0].Episode != 2 || e.Episodes[0].Status != Done || e.Episodes[0].Hash != "A1A1456789ABCDEF0123456789ABCDEF" {
		t.Fatalf("one episode got: %+v", e)
	}
	if e, _ := s.Get("tmdb-tv-2:s1e0"); e.Follow || e.Status != Done {
		t.Fatalf("whole season in one file: %+v", e)
	}
	if e, _ := s.Get("tmdb-tv-3:s2e0"); !e.Follow || e.ToSeason != 2 || e.Status != Following {
		t.Fatalf("still wanted: %+v", e)
	}
	if _, ok := Open(path).FollowListDue(time.Now(), time.Hour); !ok {
		t.Fatal("converted, not saved, or its episodes not asked for")
	}
}
