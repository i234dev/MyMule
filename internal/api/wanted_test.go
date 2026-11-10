package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/library"
	"mymule/internal/meta"
	"mymule/internal/prefs"
	"mymule/internal/release"
	"mymule/internal/server"
	"mymule/internal/wanted"
)

// wantedSetup is a REST handler with the wish list, whose eD2K searches
// find results.
func wantedSetup(t *testing.T, results []SearchResult) (*REST, *download.Manager, *wanted.Store, *httptest.Server) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	m := download.NewManager(download.Config{TempDir: tmp})
	rest := NewREST(m, restKey, "test")
	data := t.TempDir()
	rest.EnableMedia(fakeSource{})
	rest.EnableLibrary(prefs.Open(filepath.Join(data, "preferences.json"), data),
		library.OpenStore(filepath.Join(data, "library.json")), nil)
	ws := wanted.Open(filepath.Join(data, "wanted.json"))
	rest.EnableWanted(ws, t.Logf)
	rest.testSearch = func(context.Context, server.Query) ([]SearchResult, error) { return results, nil }
	srv := httptest.NewServer(rest)
	t.Cleanup(func() {
		srv.Close()
		_, files := m.Status()
		for _, f := range files {
			m.Cancel(f.Hash)
		}
	})
	return rest, m, ws, srv
}

func result(name, hash string, sources uint32) SearchResult {
	h, _ := ed2k.ParseHash(hash)
	l := ed2k.Link{Name: name, Size: 700 << 20, Hash: h}
	return SearchResult{Hash: h, Name: name, Size: 700 << 20, Sources: sources, Complete: sources, Link: l.String()}
}

// A wanted movie is searched for and its best file downloaded; when that
// download is cancelled, it is wanted again and another file is taken,
// never the cancelled one.
func TestWishList(t *testing.T) {
	results := []SearchResult{
		result("The.Film.1999.1080p.mkv", "AAAA456789ABCDEF0123456789ABCDEF", 9),
		result("The.Film.1999.720p.mkv", "BBBB456789ABCDEF0123456789ABCDEF", 4),
	}
	rest, m, ws, srv := wantedSetup(t, results)

	var added struct{ Entry wanted.Entry }
	if code := call(t, srv, "POST", "/api/v1/wanted", "application/json", `{"work_id":"tmdb-movie-1","lang":"es-ES"}`, &added); code != 200 ||
		added.Entry.Title != "La Película" || added.Entry.Status != wanted.Wanted {
		t.Fatalf("add: %d %+v", code, added.Entry)
	}
	var list struct{ Wanted []wanted.Entry }
	if call(t, srv, "GET", "/api/v1/wanted", "", "", &list); len(list.Wanted) != 1 {
		t.Fatalf("list: %+v", list)
	}
	id := added.Entry.ID

	search := func() wanted.Entry {
		t.Helper()
		rest.checkWantedDownloads()
		e, ok := ws.Due(time.Now(), time.Hour)
		if !ok {
			t.Fatal("entry not due")
		}
		rest.searchWantedEntry(context.Background(), e)
		e, _ = ws.Get(id)
		return e
	}
	var taken []string
	for round := range 2 {
		e := search()
		if e.Status != wanted.Downloading || e.Hash == "" {
			t.Fatalf("round %d: not downloading: %+v", round, e)
		}
		for _, h := range taken {
			if h == e.Hash {
				t.Fatalf("round %d: cancelled file taken again", round)
			}
		}
		taken = append(taken, e.Hash)
		h, _ := ed2k.ParseHash(e.Hash)
		if present, _ := m.Has(h); !present {
			t.Fatalf("round %d: not added to the downloads", round)
		}
		if _, ok := ws.Due(time.Now(), 0); ok {
			t.Fatal("an entry downloading is due")
		}
		m.Cancel(h) // the user did not want that one
		rest.checkWantedDownloads()
		if e, _ := ws.Get(id); e.Status != wanted.Wanted || len(e.Skip) != round+1 {
			t.Fatalf("round %d: not wanted again after cancelling: %+v", round, e)
		}
	}
	// Both files cancelled: nothing more to take.
	if e := search(); e.Status != wanted.Wanted || e.Searches != 3 {
		t.Fatalf("after all cancelled: %+v", e)
	}

	// A download that finishes marks it done.
	ws.Found(id, "CCCC456789ABCDEF0123456789ABCDEF", "x.mkv", time.Now())
	h, _ := ed2k.ParseHash("CCCC456789ABCDEF0123456789ABCDEF")
	rest.WantedCompleted(h)
	if e, _ := ws.Get(id); e.Status != wanted.Done {
		t.Fatalf("not done: %+v", e)
	}

	if code := call(t, srv, "POST", "/api/v1/wanted/"+id+"/search", "", "", nil); code != 200 {
		t.Fatalf("search now: %d", code)
	}
	if code := call(t, srv, "DELETE", "/api/v1/wanted/"+id, "", "", nil); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	if code := call(t, srv, "DELETE", "/api/v1/wanted/"+id, "", "", nil); code != 404 {
		t.Fatalf("remove again: %d", code)
	}
}

// Files without sources, hidden by the preferences or cancelled before
// are not taken.
func TestWantedChoices(t *testing.T) {
	list := []Release{
		{Hash: "11", Candidate: candidate(true, 9)},
		{Hash: "22", Candidate: candidate(false, 0)},
		{Hash: "33", Candidate: candidate(false, 5)},
		{Hash: "44", Candidate: candidate(false, 3)},
	}
	if c := wantedChoices(list, wanted.Entry{Skip: []string{"33"}}); len(c) != 1 || c[0].Hash != "44" {
		t.Fatalf("choices %+v", c)
	}
	cam := Release{Hash: "55", Candidate: candidate(false, 9)}
	cam.Info.Source = "cam"
	if c := wantedChoices(append([]Release{cam}, list[3:]...), wanted.Entry{}); len(c) != 1 || c[0].Hash != "44" {
		t.Fatalf("choices %+v: a cinema copy", c)
	}
	if c := wantedChoices(list[:2], wanted.Entry{}); len(c) != 0 {
		t.Fatal("picked a hidden file or one without sources")
	}
}

func candidate(hidden bool, sources int) release.Candidate {
	return release.Candidate{Hidden: hidden, Sources: sources}
}

// Adding an entry wakes the searches up at once.
func TestWishListWakes(t *testing.T) {
	old := wantedFirst
	wantedFirst = time.Hour
	defer func() { wantedFirst = old }()
	rest, _, ws, srv := wantedSetup(t, []SearchResult{result("The.Film.1999.1080p.mkv", "AAAA456789ABCDEF0123456789ABCDEF", 9)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rest.RunWanted(ctx)
	call(t, srv, "POST", "/api/v1/wanted", "application/json", `{"work_id":"tmdb-movie-1"}`, nil)
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if e, _ := ws.Get("tmdb-movie-1"); e.Status == wanted.Downloading {
			break
		}
		if time.Now().After(end) {
			t.Fatal("not searched for after being added")
		}
	}
}

// Available: tmdb-movie-9 comes out at home in ten days; nothing else is
// dated.
func (fakeSource) Available(_ context.Context, id string, _, _ int) (meta.Availability, bool, error) {
	if id == "tmdb-movie-9" {
		return meta.Availability{Date: time.Now().Add(10 * 24 * time.Hour), Kind: "digital"}, true, nil
	}
	return meta.Availability{}, false, nil
}

// A movie not out at home yet is not searched for, unless the user asks;
// the release search says when it comes out.
func TestWishListWaitsForRelease(t *testing.T) {
	_, _, ws, srv := wantedSetup(t, []SearchResult{result("The.Film.1999.CAM.mkv", "AAAA456789ABCDEF0123456789ABCDEF", 9)})
	var added struct{ Entry wanted.Entry }
	call(t, srv, "POST", "/api/v1/wanted", "application/json", `{"work_id":"tmdb-movie-9"}`, &added)
	if added.Entry.NotBeforeIs != "digital" || added.Entry.NotBefore.Before(time.Now()) {
		t.Fatalf("no release date: %+v", added.Entry)
	}
	if _, ok := ws.Due(time.Now(), 0); ok {
		t.Fatal("searched for before it is out")
	}
	call(t, srv, "POST", "/api/v1/wanted/tmdb-movie-9/search", "", "", nil)
	if e, ok := ws.Due(time.Now(), time.Hour); !ok || e.ID != "tmdb-movie-9" {
		t.Fatal("not searched for when asked")
	}
	var rel struct {
		Available *meta.Availability `json:"available"`
	}
	if code := call(t, srv, "GET", "/api/v1/media/tmdb-movie-9/releases", "", "", &rel); code != 200 || rel.Available == nil || rel.Available.Kind != "digital" {
		t.Fatalf("release search: %d %+v", code, rel.Available)
	}
}
