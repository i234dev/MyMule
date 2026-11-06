package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mymule/internal/api"
	"mymule/internal/authlimit"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/server"
)

const testKey = "arr-test-key-0123456789"

// Fake search results, as if from the eD2K network.
var fakeResults = []api.SearchResult{
	{Hash: ed2k.Hash{1}, Name: "Example Show S01E02 720p (demo).mkv", Size: 400_000_000, Sources: 12, Complete: 10},
	{Hash: ed2k.Hash{2}, Name: "Example Show 1x03 (demo).avi", Size: 350_000_000, Sources: 5, Complete: 5},
	{Hash: ed2k.Hash{3}, Name: "Example Movie 2024 1080p (demo).mkv", Size: 2_000_000_000, Sources: 30, Complete: 28},
	{Hash: ed2k.Hash{4}, Name: "Example Book (demo).epub", Size: 2_000_000, Sources: 3, Complete: 3},
	{Hash: ed2k.Hash{5}, Name: "Example Movie 2024 2160p (no sources).mkv", Size: 9_000_000_000},
}

type fixture struct {
	srv       *httptest.Server
	a         *Adapter
	m         *download.Manager
	downloads string
	searches  []server.Query
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	fx := &fixture{downloads: t.TempDir()}
	fx.m = download.NewManager(download.Config{TempDir: tmp, IncomingDir: fx.downloads})
	rest := api.NewREST(fx.m, testKey, "test")
	a := New(Config{Manager: fx.m, REST: rest, Key: testKey, DownloadDir: fx.downloads,
		StorePath: filepath.Join(t.TempDir(), "arr.json")})
	fx.a = a
	a.search = func(ctx context.Context, q server.Query, global bool) ([]api.SearchResult, error) {
		fx.searches = append(fx.searches, q)
		return fakeResults, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", rest)
	a.Routes(mux)
	fx.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		fx.srv.Close()
		_, files := fx.m.Status()
		for _, f := range files {
			fx.m.Cancel(f.Hash)
		}
	})
	return fx
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, c *http.Client, u string, form url.Values) (int, string) {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTorznab(t *testing.T) {
	fx := newFixture(t)
	c := http.DefaultClient
	base := fx.srv.URL + "/torznab/api"

	if code, _ := get(t, c, base+"?t=caps"); code != 401 {
		t.Fatalf("caps without key: %d", code)
	}
	code, body := get(t, c, base+"?t=caps&apikey="+testKey)
	if code != 200 || !strings.Contains(body, `<tv-search available="yes" supportedParams="q,season,ep"/>`) {
		t.Fatalf("caps: %d %s", code, body)
	}

	// Empty query (indexer test / RSS): one clearly labelled test item.
	_, body = get(t, c, base+"?t=search&apikey="+testKey)
	if strings.Count(body, "<item>") != 1 || !strings.Contains(body, "MyMule indexer test") {
		t.Fatalf("empty search: %s", body)
	}

	// A TV search keeps only the requested episode, in either notation.
	_, body = get(t, c, base+"?t=tvsearch&q=Example+Show&season=1&ep=2&apikey="+testKey)
	if strings.Count(body, "<item>") != 1 || !strings.Contains(body, "S01E02") {
		t.Fatalf("tvsearch S01E02: %s", body)
	}
	_, body = get(t, c, base+"?t=tvsearch&q=Example+Show&season=1&ep=3&apikey="+testKey)
	if strings.Count(body, "<item>") != 1 || !strings.Contains(body, "1x03") {
		t.Fatalf("tvsearch 1x03: %s", body)
	}
	if last := fx.searches[len(fx.searches)-1]; last.Type != server.TypeVideo || last.Keywords != "Example Show" {
		t.Fatalf("tv search sent to eD2K: %+v", last)
	}

	// Category filter: movies only. The result without sources is dropped.
	_, body = get(t, c, base+"?t=movie&q=Example&cat=2000,2040&apikey="+testKey)
	if strings.Count(body, "<item>") != 1 || !strings.Contains(body, "Example Movie 2024") {
		t.Fatalf("movie search: %s", body)
	}
	for _, want := range []string{`xmlns:torznab="http://torznab.com/schemas/2015/feed"`,
		`<torznab:attr name="seeders" value="28"/>`, `<torznab:attr name="infohash" value="` + InfoHash(ed2k.Link{Name: fakeResults[2].Name, Size: int64(fakeResults[2].Size), Hash: fakeResults[2].Hash}) + `"/>`,
		`type="application/x-bittorrent"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("feed lacks %s:\n%s", want, body)
		}
	}

	// The enclosure URL serves a stand-in torrent with the ed2k link.
	start := strings.Index(body, `<enclosure url="`) + len(`<enclosure url="`)
	encl := strings.ReplaceAll(body[start:start+strings.Index(body[start:], `"`)], "&amp;", "&")
	code, tor := get(t, c, encl)
	if code != 200 {
		t.Fatalf("torrent download: %d %s", code, tor)
	}
	l, err := LinkFromTorrent([]byte(tor))
	if err != nil || l.Hash != fakeResults[2].Hash || l.Name != fakeResults[2].Name {
		t.Fatalf("torrent content: %+v %v", l, err)
	}
}

func TestQBittorrentFlow(t *testing.T) {
	fx := newFixture(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	api2 := fx.srv.URL + "/api/v2"

	if code, _ := get(t, c, api2+"/app/webapiVersion"); code != 403 {
		t.Fatalf("without login: %d", code)
	}
	if _, body := post(t, c, api2+"/auth/login", url.Values{"username": {"radarr"}, "password": {"wrong"}}); body != "Fails." {
		t.Fatalf("bad login: %s", body)
	}
	if _, body := post(t, c, api2+"/auth/login", url.Values{"username": {"radarr"}, "password": {testKey}}); body != "Ok." {
		t.Fatalf("login: %s", body)
	}
	if _, v := get(t, c, api2+"/app/webapiVersion"); v != qbitAPIVersion {
		t.Fatalf("api version: %s", v)
	}
	_, prefs := get(t, c, api2+"/app/preferences")
	if !strings.Contains(prefs, `"save_path"`) {
		t.Fatalf("prefs: %s", prefs)
	}
	post(t, c, api2+"/torrents/createCategory", url.Values{"category": {"radarr"}, "savePath": {""}})
	if _, cats := get(t, c, api2+"/torrents/categories"); !strings.Contains(cats, `"radarr"`) {
		t.Fatalf("categories: %s", cats)
	}

	// Radarr uploads the stand-in torrent it got from the indexer.
	link := ed2k.Link{Name: "Example Movie 2024 1080p (demo).mkv", Size: 2_000_000_000, Hash: ed2k.Hash{3}}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("torrents", "movie.torrent")
	fw.Write(Torrent(link))
	mw.WriteField("category", "radarr")
	mw.WriteField("savepath", "")
	mw.Close()
	req, _ := http.NewRequest("POST", api2+"/torrents/add", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	added, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(added) != "Ok." {
		t.Fatalf("add: %d %s", resp.StatusCode, added)
	}

	// Plain ed2k links work too, in another category.
	post(t, c, api2+"/torrents/add", url.Values{"urls": {"ed2k://|file|Example Show S01E02 (demo).mkv|400000000|01000000000000000000000000000000|/"}, "category": {"sonarr"}})

	var info []map[string]any
	_, js := get(t, c, api2+"/torrents/info?category=radarr")
	json.Unmarshal([]byte(js), &info)
	if len(info) != 1 {
		t.Fatalf("info radarr: %s", js)
	}
	ih := InfoHash(link)
	if info[0]["hash"] != ih || info[0]["state"] != "stalledDL" || info[0]["save_path"] != fx.downloads ||
		info[0]["category"] != "radarr" || info[0]["content_path"] != filepath.Join(fx.downloads, link.Name) {
		t.Fatalf("torrent: %+v", info[0])
	}
	_, js = get(t, c, api2+"/torrents/info")
	json.Unmarshal([]byte(js), &info)
	if len(info) != 2 {
		t.Fatalf("all torrents: %s", js)
	}

	post(t, c, api2+"/torrents/pause", url.Values{"hashes": {ih}})
	_, js = get(t, c, api2+"/torrents/info?hashes="+ih)
	json.Unmarshal([]byte(js), &info)
	if info[0]["state"] != "pausedDL" {
		t.Fatalf("after pause: %v", info[0]["state"])
	}
	post(t, c, api2+"/torrents/resume", url.Values{"hashes": {ih}})
	if code, files := get(t, c, api2+"/torrents/files?hash="+ih); code != 200 || !strings.Contains(files, link.Name) {
		t.Fatalf("files: %d %s", code, files)
	}

	post(t, c, api2+"/torrents/delete", url.Values{"hashes": {ih}, "deleteFiles": {"true"}})
	_, js = get(t, c, api2+"/torrents/info?category=radarr")
	if js != "[]\n" {
		t.Fatalf("after delete: %s", js)
	}

	// Foreign torrents are refused.
	var body2 bytes.Buffer
	mw2 := multipart.NewWriter(&body2)
	fw2, _ := mw2.CreateFormFile("torrents", "other.torrent")
	fw2.Write([]byte("d4:infod4:name1:xee"))
	mw2.Close()
	req2, _ := http.NewRequest("POST", api2+"/torrents/add", &body2)
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	resp2, _ := c.Do(req2)
	refused, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(refused) != "Fails." {
		t.Fatalf("foreign torrent: %s", refused)
	}
}

func TestEpisodeAndCategory(t *testing.T) {
	cases := []struct {
		name    string
		s, e    int
		matches bool
	}{
		{"Show S01E02 720p.mkv", 1, 2, true},
		{"Show s01.e02.mkv", 1, 2, true},
		{"Show 1x02.avi", 1, 2, true},
		{"Show S01E03.mkv", 1, 2, false},
		{"Show S02E02.mkv", 1, 2, false},
		{"Show Season 1 complete.mkv", 1, 0, true},
		{"Show Temporada 2.mkv", 2, 0, true},
		{"Anything.mkv", 0, 0, true},
	}
	for _, c := range cases {
		if got := matchesEpisode(c.name, c.s, c.e); got != c.matches {
			t.Errorf("matchesEpisode(%q, %d, %d) = %v", c.name, c.s, c.e, got)
		}
	}
	for name, want := range map[string]int{"Show S01E02.mkv": catTV, "Movie 2024.mkv": catMovies,
		"Album.flac": catAudio, "Book.epub": catBooks, "distro.iso": catPC, "notes.xyz": catOther} {
		if got := category(name, ""); got != want {
			t.Errorf("category(%q) = %d, want %d", name, got, want)
		}
	}
}

// The qBittorrent login and the Torznab apikey share the guessing limit.
func TestArrGuessingBlocked(t *testing.T) {
	old := authlimit.Default
	authlimit.Default = authlimit.New(4, time.Minute, time.Minute)
	defer func() { authlimit.Default = old }()
	fx := newFixture(t)
	c := &http.Client{}
	login := fx.srv.URL + "/api/v2/auth/login"
	post(t, c, login, url.Values{"password": {"guess1"}})
	post(t, c, login, url.Values{"password": {"guess2"}})
	get(t, c, fx.srv.URL+"/torznab/api?t=caps&apikey=guess3")
	get(t, c, fx.srv.URL+"/torznab/api?t=caps&apikey=guess4")
	if _, body := post(t, c, login, url.Values{"password": {testKey}}); body != "Fails." {
		t.Fatalf("right password accepted while blocked: %q", body)
	}
	if code, _ := get(t, c, fx.srv.URL+"/torznab/api?t=caps&apikey="+testKey); code == 200 {
		t.Fatal("right apikey accepted while blocked")
	}
}

// Uploads through the qBittorrent API are bounded: a huge request is
// refused, and a torrent far bigger than MyMule's stand-ins is skipped.
func TestQBittorrentAddLimits(t *testing.T) {
	fx := newFixture(t)
	add := func(name string, content []byte) (int, string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("torrents", name)
		fw.Write(content)
		mw.Close()
		req, _ := http.NewRequest("POST", fx.srv.URL+"/api/v2/torrents/add?apikey="+testKey, &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := add("huge.torrent", bytes.Repeat([]byte("x"), maxAddBody+1)); code == 200 && body == "Ok." {
		t.Fatal("request over the size limit accepted")
	}
	// A valid torrent, just far bigger than any MyMule makes (a huge name).
	big := Torrent(ed2k.Link{Name: strings.Repeat("a", maxTorrentSize) + ".mkv", Size: 1000, Hash: ed2k.Hash{4}})
	if _, body := add("big.torrent", big); body != "Fails." {
		t.Fatalf("oversized torrent: %q", body)
	}
	if _, body := add("ok.torrent", Torrent(ed2k.Link{Name: "a.mkv", Size: 1000, Hash: ed2k.Hash{5}})); body != "Ok." {
		t.Fatalf("normal torrent: %q", body)
	}
}
