package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/ratings"
	"mymule/internal/wanted"
)

func TestSummarizeNotes(t *testing.T) {
	n := summarize([]kad.Note{{Rating: 1, Comment: "otra película"}, {Rating: 5}, {Rating: 4, Comment: "bien"}, {Comment: "sin nota"}})
	if n.Votes != 3 || n.Fake != 1 || n.Suspect || n.Average != 10.0/3 || len(n.Comments) != 3 || !n.Kad {
		t.Fatalf("%+v", n)
	}
	if n := summarize([]kad.Note{{Rating: 1}, {Rating: 4}}); !n.Suspect {
		t.Fatal("half call it fake: not suspect")
	}
	if n := summarize([]kad.Note{{Rating: 1}}); !n.Suspect {
		t.Fatal("the only vote says fake: not suspect")
	}
	if n := summarize(nil); n.Suspect || n.Votes != 0 || n.Comments == nil {
		t.Fatalf("%+v", n)
	}
}

// The notes endpoint sums up what users say, asking Kad once an hour; the
// wish list skips a file people call fake and takes the next one.
func TestNotes(t *testing.T) {
	results := []SearchResult{
		result("The.Film.1999.1080p.mkv", "AAAA456789ABCDEF0123456789ABCDEF", 9),
		result("The.Film.1999.720p.mkv", "BBBB456789ABCDEF0123456789ABCDEF", 4),
	}
	rest, _, ws, srv := wantedSetup(t, results)
	asked := 0
	rest.testNotes = func(h ed2k.Hash) ([]kad.Note, error) {
		asked++
		if h.String() == "AAAA456789ABCDEF0123456789ABCDEF" {
			return []kad.Note{{Rating: 1, Comment: "fake"}, {Rating: 1}, {Rating: 3}}, nil
		}
		return []kad.Note{{Rating: 5}}, nil
	}
	var got struct{ Notes Notes }
	for range 2 {
		if code := call(t, srv, "GET", "/api/v1/files/AAAA456789ABCDEF0123456789ABCDEF/notes?size=734003200", "", "", &got); code != 200 ||
			!got.Notes.Suspect || got.Notes.Fake != 2 || got.Notes.Votes != 3 {
			t.Fatalf("notes: %d %+v", code, got.Notes)
		}
	}
	if asked != 1 {
		t.Fatalf("Kad asked %d times", asked)
	}
	if code := call(t, srv, "GET", "/api/v1/files/AAAA456789ABCDEF0123456789ABCDEF/notes", "", "", nil); code != 400 {
		t.Fatalf("without size: %d", code)
	}

	e, _ := ws.Add(wanted.Entry{WorkID: "tmdb-movie-1", Title: "La Película", Lang: "es-ES"})
	rest.searchWantedEntry(context.Background(), e)
	if e, _ = ws.Get(e.ID); e.Status != wanted.Downloading || e.Hash != "BBBB456789ABCDEF0123456789ABCDEF" {
		t.Fatalf("took %+v, want the file not called fake", e)
	}
}

// The user rates a file; without Kad it is kept to be published later.
func TestRateFile(t *testing.T) {
	m := download.NewManager(download.Config{TempDir: t.TempDir()})
	rest := NewREST(m, restKey, "test")
	store := ratings.Open(filepath.Join(t.TempDir(), "ratings.json"))
	rest.EnableRatings(store)
	srv := httptest.NewServer(rest)
	defer srv.Close()
	path := "/api/v1/files/0123456789ABCDEF0123456789ABCDEF/rating"
	var out struct{ Saved, Published bool }
	if code := call(t, srv, "PUT", path, "application/json", `{"rating":1,"comment":"another film","name":"Example.Film.mkv","size":1000}`, &out); code != 200 || !out.Saved || out.Published {
		t.Fatalf("%d %+v", code, out)
	}
	if code := call(t, srv, "PUT", path, "application/json", `{"rating":7,"name":"x","size":1}`, nil); code != 400 {
		t.Fatalf("rating 7: %d", code)
	}
	var list struct{ Ratings map[string]ratings.Rating }
	call(t, srv, "GET", "/api/v1/ratings", "", "", &list)
	if r := list.Ratings["0123456789ABCDEF0123456789ABCDEF"]; r.Rating != 1 || r.Comment != "another film" {
		t.Fatalf("%+v", list)
	}
}
