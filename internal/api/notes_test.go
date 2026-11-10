package api

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/ratings"
	"mymule/internal/wanted"
)

// twoNets are two nodes of different networks.
var twoNets = []string{"192.0.2.1", "198.51.100.1"}

// given returns the notes of users (one each, in order) as every one of
// nodes gives them.
func given(nodes []string, notes ...kad.Note) []kad.Note {
	var out []kad.Note
	for i, n := range notes {
		n.UserHash[0], n.UserHash[1] = byte(i), byte(i>>8)
		n.UserHash[15] = 1
		for _, a := range nodes {
			n.Node = netip.MustParseAddr(a)
			out = append(out, n)
		}
	}
	return out
}

func TestSummarizeNotes(t *testing.T) {
	n := summarize(given(twoNets, kad.Note{Rating: 1, Comment: "otra película"}, kad.Note{Rating: 5},
		kad.Note{Rating: 4, Comment: "bien"}, kad.Note{Comment: "sin nota"}))
	if n.Votes != 3 || n.Fake != 1 || n.Suspect || n.Average != 10.0/3 || len(n.Comments) != 3 || !n.Kad {
		t.Fatalf("%+v", n)
	}
	if n := summarize(given(twoNets, kad.Note{Rating: 1}, kad.Note{Rating: 4})); !n.Suspect {
		t.Fatal("half call it fake: not suspect")
	}
	if n := summarize(given(twoNets, kad.Note{Rating: 1})); !n.Suspect {
		t.Fatal("the only vote says fake: not suspect")
	}
	if n := summarize(nil); n.Suspect || n.Votes != 0 || n.Comments == nil {
		t.Fatalf("%+v", n)
	}

	// What one node, or one network, says is shown but does not hide a file.
	for _, nodes := range [][]string{{"192.0.2.1"}, {"192.0.2.1", "192.0.2.77"}, {"2001:db8::1", "2001:db8::2"}} {
		if n := summarize(given(nodes, kad.Note{Rating: 1, Comment: "fake"})); n.Suspect || n.Fake != 1 || n.Votes != 1 {
			t.Errorf("%v: %+v", nodes, n)
		}
	}
	// A node making up many users that call it fake.
	list := given(twoNets, kad.Note{Rating: 5})
	var made []kad.Note
	for range 50 {
		made = append(made, kad.Note{Rating: 1})
	}
	for _, m := range given([]string{"203.0.113.9"}, made...) {
		m.UserHash[15] = 2 // other users than the real one
		list = append(list, m)
	}
	if n := summarize(list); n.Suspect || n.Votes != 51 {
		t.Fatalf("made-up users: %+v", n)
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
			return given(twoNets, kad.Note{Rating: 1, Comment: "fake"}, kad.Note{Rating: 1}, kad.Note{Rating: 3}), nil
		}
		return given(twoNets, kad.Note{Rating: 5}), nil
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
