package api

import (
	"context"
	"fmt"
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

// Before a movie is out at home, its web and disc copies cannot exist:
// they are hidden as fakes, and not recommended. A name that says nothing
// stays.
func TestTooEarlyHidden(t *testing.T) {
	_, _, _, srv := wantedSetup(t, []SearchResult{
		result("The.Film.1999.1080p.AMZN.WEB-DL.mkv", "AAAA456789ABCDEF0123456789ABCDEF", 50),
		result("The.Film.1999.BluRay.1080p.mkv", "BBBB456789ABCDEF0123456789ABCDEF", 40),
		result("The.Film.1999.HDTC.mkv", "CCCC456789ABCDEF0123456789ABCDEF", 30),
		result("The.Film.1999.1080p.mkv", "DDDD456789ABCDEF0123456789ABCDEF", 3),
	})
	var rel struct {
		Releases []Release `json:"releases"`
	}
	if code := call(t, srv, "GET", "/api/v1/media/tmdb-movie-9/releases", "", "", &rel); code != 200 || len(rel.Releases) != 4 {
		t.Fatalf("%d %+v", code, rel.Releases)
	}
	why := map[string]string{}
	for _, x := range rel.Releases {
		why[x.Hash] = x.HiddenWhy
		if x.Recommended && x.Hash != "DDDD456789ABCDEF0123456789ABCDEF" {
			t.Errorf("%s recommended", x.Name)
		}
	}
	if why["AAAA456789ABCDEF0123456789ABCDEF"] != tooEarly || why["BBBB456789ABCDEF0123456789ABCDEF"] != tooEarly ||
		why["DDDD456789ABCDEF0123456789ABCDEF"] != "" || why["CCCC456789ABCDEF0123456789ABCDEF"] == "" {
		t.Fatalf("%v", why)
	}
	// Out at home already (tmdb-movie-1 has no date): nothing hidden for it.
	var out struct {
		Releases []Release `json:"releases"`
	}
	call(t, srv, "GET", "/api/v1/media/tmdb-movie-1/releases", "", "", &out)
	for _, x := range out.Releases {
		if x.HiddenWhy == tooEarly {
			t.Fatalf("%s hidden without a release date", x.Name)
		}
	}
}

// The user can choose the day to start searching, or go back to the date
// the title comes out; the daily date check keeps a chosen date.
func TestWishListManualDate(t *testing.T) {
	_, _, ws, srv := wantedSetup(t, nil)
	future := time.Now().AddDate(0, 1, 0).Format(time.DateOnly)
	var got struct{ Entry wanted.Entry }
	if code := call(t, srv, "POST", "/api/v1/wanted", "application/json", `{"work_id":"tmdb-movie-1","not_before":"`+future+`"}`, &got); code != 200 ||
		got.Entry.NotBeforeIs != wanted.Manual || got.Entry.NotBefore.Format(time.DateOnly) != future {
		t.Fatalf("%d %+v", code, got.Entry)
	}
	if _, ok := ws.Due(time.Now(), 0); ok {
		t.Fatal("searched for before the chosen day")
	}
	if _, ok := ws.DateDue(time.Now(), 0); ok {
		t.Fatal("a chosen date is checked again")
	}
	ws.Dated("tmdb-movie-1", time.Now(), "digital", time.Now())
	if e, _ := ws.Get("tmdb-movie-1"); e.NotBeforeIs != wanted.Manual {
		t.Fatal("chosen date replaced")
	}
	past := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
	call(t, srv, "POST", "/api/v1/wanted/tmdb-movie-1/date", "application/json", `{"date":"`+past+`"}`, nil)
	if _, ok := ws.Due(time.Now(), time.Hour); !ok {
		t.Fatal("not due after its chosen day")
	}
	if code := call(t, srv, "POST", "/api/v1/wanted/tmdb-movie-1/date", "application/json", `{"date":"tomorrow"}`, nil); code != 400 {
		t.Fatalf("bad date: %d", code)
	}

	// Back to automatic: tmdb-movie-9 comes out in ten days.
	call(t, srv, "POST", "/api/v1/wanted", "application/json", `{"work_id":"tmdb-movie-9","not_before":"`+past+`"}`, nil)
	if code := call(t, srv, "POST", "/api/v1/wanted/tmdb-movie-9/date", "application/json", `{"date":""}`, &got); code != 200 ||
		got.Entry.NotBeforeIs != "digital" || got.Entry.NotBefore.Before(time.Now()) {
		t.Fatalf("%d %+v", code, got.Entry)
	}
}

// seriesSource is fakeSource with seasons: season 1 aired, season 2 half.
type seriesSource struct{ fakeSource }

func (seriesSource) Get(ctx context.Context, id, lang string) (meta.Work, error) {
	w, err := fakeSource{}.Get(ctx, id, lang)
	w.Seasons = []meta.Season{{Number: 1, Episodes: 3}, {Number: 2, Episodes: 2}}
	return w, err
}

func (seriesSource) SeasonEpisodes(_ context.Context, _ string, season int, _ string) ([]meta.Episode, error) {
	past, future := time.Now().AddDate(0, -1, 0).Format(time.DateOnly), time.Now().AddDate(0, 0, 7).Format(time.DateOnly)
	if season == 1 {
		return []meta.Episode{{Number: 1, AirDate: past}, {Number: 2, AirDate: past}, {Number: 3, AirDate: past}}, nil
	}
	return []meta.Episode{{Number: 1, AirDate: past}, {Number: 2, AirDate: future}}, nil
}

// A followed series gets every aired episode, a season per search, the
// best file of each (never a cinema copy); one not aired yet waits; a
// cancelled one is searched for again and its file not taken again.
func TestFollowSeries(t *testing.T) {
	rest, m, ws, srv := wantedSetup(t, []SearchResult{
		result("The.Show.S01E01.1080p.mkv", "A1A1456789ABCDEF0123456789ABCDEF", 9),
		result("The.Show.S01E01.720p.mkv", "A2A2456789ABCDEF0123456789ABCDEF", 5),
		result("The.Show.S01E02.CAM.mkv", "B1B1456789ABCDEF0123456789ABCDEF", 30),
		result("The.Show.S01E02.1080p.mkv", "B2B2456789ABCDEF0123456789ABCDEF", 4),
		result("The.Show.S02E01.1080p.mkv", "C1C1456789ABCDEF0123456789ABCDEF", 7),
		result("The.Show.S02E02.1080p.mkv", "D1D1456789ABCDEF0123456789ABCDEF", 7), // not aired: a fake
	})
	rest.media = seriesSource{}
	var got struct{ Entry wanted.Entry }
	if code := call(t, srv, "POST", "/api/v1/wanted", "application/json", `{"work_id":"tmdb-tv-1","follow":true}`, &got); code != 200 ||
		!got.Entry.Follow || len(got.Entry.Episodes) != 5 {
		t.Fatalf("%d %+v", code, got.Entry)
	}
	id := got.Entry.ID
	searchDue := func() int {
		t.Helper()
		e, season, ok := ws.FollowDue(time.Now(), time.Hour)
		if !ok {
			return 0
		}
		rest.searchFollowSeason(context.Background(), e, season)
		return season
	}
	if s := searchDue(); s != 1 {
		t.Fatalf("searched season %d first", s)
	}
	if s := searchDue(); s != 2 {
		t.Fatalf("then season %d", s)
	}
	if s := searchDue(); s != 0 {
		t.Fatalf("season %d searched again too soon", s)
	}
	e, _ := ws.Get(id)
	want := map[string]string{"1x1": "A1A1456789ABCDEF0123456789ABCDEF", "1x2": "B2B2456789ABCDEF0123456789ABCDEF", "1x3": "", "2x1": "C1C1456789ABCDEF0123456789ABCDEF", "2x2": ""}
	for _, ep := range e.Episodes {
		k := fmt.Sprintf("%dx%d", ep.Season, ep.Episode)
		if ep.Hash != want[k] {
			t.Errorf("%s: took %q, want %q (%s)", k, ep.Hash, want[k], ep.Status)
		}
	}

	// A season on the air: the aired episode's file is there, the next
	// one's (it airs next week) is a fake.
	found, err := rest.findReleases(context.Background(), "tmdb-tv-1", "en", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range found.releases {
		if aired := x.Info.Episode == 1; aired == (x.HiddenWhy == tooEarly) {
			t.Errorf("%s: hidden %q", x.Name, x.HiddenWhy)
		}
	}

	// Cancelled: wanted again, its season searched again, the other file
	// taken.
	h, _ := ed2k.ParseHash("A1A1456789ABCDEF0123456789ABCDEF")
	m.Cancel(h)
	rest.checkWantedDownloads()
	if s := searchDue(); s != 1 {
		t.Fatalf("season %d searched after a cancel", s)
	}
	e, _ = ws.Get(id)
	if e.Episodes[0].Hash != "A2A2456789ABCDEF0123456789ABCDEF" {
		t.Fatalf("after cancelling: %+v", e.Episodes[0])
	}
	// A finished download marks its episode done.
	rest.WantedCompleted(ed2k.Hash(mustHash(t, "C1C1456789ABCDEF0123456789ABCDEF")))
	if e, _ = ws.Get(id); e.Episodes[3].Status != wanted.Done {
		t.Fatalf("not done: %+v", e.Episodes[3])
	}
}

func mustHash(t *testing.T, s string) ed2k.Hash {
	t.Helper()
	h, err := ed2k.ParseHash(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Available: like TMDB, season 2 "airs" when its last episode does.
func (seriesSource) Available(_ context.Context, _ string, season, episode int) (meta.Availability, bool, error) {
	if season == 2 && (episode == 0 || episode == 2) {
		return meta.Availability{Date: time.Now().AddDate(0, 0, 7), Kind: "aired"}, true, nil
	}
	return meta.Availability{}, false, nil
}

// A whole season on the wish list gets every episode of it, the best file
// of each; an episode with no file of its own takes a file with the whole
// season; it is done when they all are, and other seasons are left alone.
func TestWishSeason(t *testing.T) {
	rest, _, ws, srv := wantedSetup(t, []SearchResult{
		result("The.Show.S01E01.1080p.mkv", "A1A1456789ABCDEF0123456789ABCDEF", 9),
		result("The.Show.S01E02.1080p.mkv", "B2B2456789ABCDEF0123456789ABCDEF", 4),
		result("The.Show.S01.Complete.1080p.mkv", "F1F1456789ABCDEF0123456789ABCDEF", 3),
		result("The.Show.S02E01.1080p.mkv", "C1C1456789ABCDEF0123456789ABCDEF", 7),
	})
	rest.media = seriesSource{}
	var got struct{ Entry wanted.Entry }
	if code := call(t, srv, "POST", "/api/v1/wanted", "application/json", `{"work_id":"tmdb-tv-1","season":1}`, &got); code != 200 ||
		!got.Entry.Follow || got.Entry.ToSeason != 1 || len(got.Entry.Episodes) != 3 {
		t.Fatalf("%d %+v", code, got.Entry)
	}
	id := got.Entry.ID
	e, season, ok := ws.FollowDue(time.Now(), time.Hour)
	if !ok || season != 1 {
		t.Fatalf("due: %v season %d", ok, season)
	}
	rest.searchFollowSeason(context.Background(), e, season)
	e, _ = ws.Get(id)
	want := []string{"A1A1456789ABCDEF0123456789ABCDEF", "B2B2456789ABCDEF0123456789ABCDEF", "F1F1456789ABCDEF0123456789ABCDEF"}
	for i, ep := range e.Episodes {
		if ep.Hash != want[i] || ep.Status != wanted.Downloading {
			t.Errorf("S01E%02d: %q %s, want %q", ep.Episode, ep.Hash, ep.Status, want[i])
		}
	}
	if _, _, ok := ws.FollowDue(time.Now(), time.Hour); ok {
		t.Fatal("something else due: only season 1 is wanted")
	}
	for _, h := range want[:2] {
		rest.WantedCompleted(mustHash(t, h))
	}
	if e, _ = ws.Get(id); e.Status == wanted.Done {
		t.Fatal("done with an episode still downloading")
	}
	rest.WantedCompleted(mustHash(t, want[2]))
	if e, _ = ws.Get(id); e.Status != wanted.Done {
		t.Fatalf("not done: %+v", e)
	}
	if _, ok := ws.FollowListDue(time.Now().Add(48*time.Hour), time.Hour); ok {
		t.Fatal("a finished season still asks for its episodes")
	}
}
