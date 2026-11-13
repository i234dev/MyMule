package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mymule/internal/authlimit"
	"mymule/internal/download"
	"mymule/internal/stats"
)

const restKey = "test-key-0123456789abcdef"

const (
	linkA = "ed2k://|file|example-a.iso|20000000|0123456789ABCDEF0123456789ABCDEF|/"
	linkB = "ed2k://|file|example-b.pdf|3000000|FEDCBA9876543210FEDCBA9876543210|/"
)

func newTestREST(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	m := download.NewManager(download.Config{TempDir: tmp})
	srv := httptest.NewServer(NewREST(m, restKey, "v9.9.9-test"))
	t.Cleanup(func() {
		srv.Close()
		// Close open .part files so the temp dir can be removed (Windows).
		_, files := m.Status()
		for _, f := range files {
			m.Cancel(f.Hash)
		}
	})
	return srv, tmp
}

// call makes a request with the key in the header and decodes JSON.
func call(t *testing.T, srv *httptest.Server, method, path, ctype, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Api-Key", restKey)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: bad JSON %q", method, path, data)
		}
	}
	return resp.StatusCode
}

func TestRESTAuth(t *testing.T) {
	srv, _ := newTestREST(t)
	get := func(path string, hdr map[string]string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/api/v1/version", nil); c != 401 {
		t.Fatalf("no key: %d", c)
	}
	if c := get("/api/v1/version", map[string]string{"X-Api-Key": "wrong"}); c != 401 {
		t.Fatalf("wrong key: %d", c)
	}
	if c := get("/api/v1/version", map[string]string{"X-Api-Key": restKey}); c != 200 {
		t.Fatalf("header key: %d", c)
	}
	if c := get("/api/v1/version", map[string]string{"Authorization": "Bearer " + restKey}); c != 200 {
		t.Fatalf("bearer key: %d", c)
	}
	if c := get("/api/v1/version?apikey="+restKey, nil); c != 200 {
		t.Fatalf("query key: %d", c)
	}
	// An empty configured key never authorises anything.
	empty := httptest.NewServer(NewREST(download.NewManager(download.Config{}), "", "x"))
	defer empty.Close()
	req, _ := http.NewRequest("GET", empty.URL+"/api/v1/version?apikey=", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("empty key accepted: %d", resp.StatusCode)
	}
}

func TestRESTDownloads(t *testing.T) {
	srv, tmp := newTestREST(t)

	var ver map[string]string
	if call(t, srv, "GET", "/api/v1/version", "", "", &ver); ver["version"] != "v9.9.9-test" {
		t.Fatalf("version: %v", ver)
	}

	// JSON add: one good link, one bad.
	var add AddResponse
	body := `{"links": ["` + linkA + `", "not a link"]}`
	if c := call(t, srv, "POST", "/api/v1/downloads", "application/json", body, &add); c != 200 ||
		len(add.Added) != 1 || len(add.Errors) != 1 {
		t.Fatalf("json add: %d %+v", c, add)
	}
	// Form add, the easy way from PHP (http_build_query) or curl -d.
	form := url.Values{"link": {linkB}}.Encode()
	if c := call(t, srv, "POST", "/api/v1/downloads", "application/x-www-form-urlencoded", form, &add); c != 200 || len(add.Added) != 1 {
		t.Fatalf("form add: %d %+v", c, add)
	}
	// Adding nothing valid is a 400.
	if c := call(t, srv, "POST", "/api/v1/downloads", "application/json", `{"links":["x"]}`, &add); c != 400 {
		t.Fatalf("bad add: %d", c)
	}

	var list struct{ Downloads []RDownload }
	call(t, srv, "GET", "/api/v1/downloads", "", "", &list)
	if len(list.Downloads) != 2 {
		t.Fatalf("list: %+v", list)
	}
	a := list.Downloads[0]
	if a.Name != "example-a.iso" || a.Size != 20000000 || a.State != "stalled" || a.Link != linkA || a.ETA != -1 {
		t.Fatalf("download a: %+v", a)
	}

	var one RDownload
	if c := call(t, srv, "GET", "/api/v1/downloads/"+a.Hash, "", "", &one); c != 200 || one.Name != a.Name {
		t.Fatalf("get one: %d %+v", c, one)
	}
	var e ErrorResponse
	if c := call(t, srv, "GET", "/api/v1/downloads/xyz", "", "", &e); c != 400 {
		t.Fatalf("bad hash: %d", c)
	}
	if c := call(t, srv, "GET", "/api/v1/downloads/00000000000000000000000000000000", "", "", &e); c != 404 {
		t.Fatalf("unknown hash: %d", c)
	}

	var ok map[string]any
	call(t, srv, "POST", "/api/v1/downloads/"+a.Hash+"/pause", "", "", &ok)
	call(t, srv, "GET", "/api/v1/downloads/"+a.Hash, "", "", &one)
	if one.State != "paused" {
		t.Fatalf("after pause: %s", one.State)
	}
	call(t, srv, "POST", "/api/v1/downloads/"+a.Hash+"/resume", "", "", &ok)
	call(t, srv, "GET", "/api/v1/downloads/"+a.Hash, "", "", &one)
	if one.State == "paused" {
		t.Fatal("still paused after resume")
	}

	if c := call(t, srv, "DELETE", "/api/v1/downloads/"+a.Hash, "", "", &ok); c != 200 || ok["data_deleted"] != true {
		t.Fatalf("cancel: %d %v", c, ok)
	}
	if _, err := os.Stat(filepath.Join(tmp, a.Hash+".part")); !os.IsNotExist(err) {
		t.Fatal("cancelled download still on disk")
	}

	var cleared map[string]int
	if call(t, srv, "POST", "/api/v1/downloads/clear", "", "", &cleared); cleared["removed"] != 0 {
		t.Fatalf("clear: %v", cleared)
	}

	var st RStatus
	if call(t, srv, "GET", "/api/v1/status", "", "", &st); len(st.Downloads) != 1 || st.Server.Connected {
		t.Fatalf("status: %+v", st)
	}

	// Search without a server is a clear error, not a hang.
	if c := call(t, srv, "GET", "/api/v1/search?q=example&global=false", "", "", &e); c != 503 || e.Error == "" {
		t.Fatalf("search without server: %d %+v", c, e)
	}
	if c := call(t, srv, "GET", "/api/v1/search", "", "", &e); c != 400 {
		t.Fatalf("search without q: %d", c)
	}
	if c := call(t, srv, "GET", "/api/v1/nothing", "", "", &e); c != 404 {
		t.Fatalf("unknown endpoint: %d", c)
	}
}

// Guessing the key gets the address blocked, whichever way the key is
// sent. While blocked, a short (guessable) key is refused even when right;
// a long random one, like the generated key, still works, so nobody can
// lock the user out. Requests without any key (an expired page polling)
// do not count.
func TestRESTGuessingBlocked(t *testing.T) {
	old := authlimit.Default
	defer func() { authlimit.Default = old }()
	const weakKey = "short-key-12"
	for _, key := range []string{weakKey, restKey} {
		authlimit.Default = authlimit.New(5, time.Minute, time.Minute)
		srv := httptest.NewServer(NewREST(download.NewManager(download.Config{}), key, "x"))
		get := func(path string, hdr map[string]string) int {
			req, _ := http.NewRequest("GET", srv.URL+path, nil)
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		for i := 0; i < 20; i++ {
			get("/api/v1/version", nil) // no key: not an attempt
		}
		if c := get("/api/v1/version", map[string]string{"X-Api-Key": key}); c != 200 {
			t.Fatalf("keyless requests blocked the address: %d", c)
		}
		for i := 0; i < 5; i++ {
			switch i % 3 {
			case 0:
				get("/api/v1/version", map[string]string{"X-Api-Key": "guess"})
			case 1:
				get("/api/v1/version", map[string]string{"Authorization": "Bearer guess"})
			default:
				get("/api/v1/version?apikey=guess", nil)
			}
		}
		c := get("/api/v1/version", map[string]string{"X-Api-Key": key})
		if key == weakKey && c != 401 {
			t.Fatalf("right short key accepted while blocked: %d", c)
		}
		if key == restKey && c != 200 {
			t.Fatalf("right long key refused while blocked: %d", c)
		}
		srv.Close()
	}
}

// A web page the user opens can make the browser send requests with a
// made-up key (<img src=...?apikey=x>, a hidden form). Wrong keys sent
// that way are counted apart, so the page cannot block the user's own
// address; but anyone can make a request look cross-site, so they stop
// further cross-site tries once too many (or a short key could be
// guessed without limit).
func TestRESTCrossSiteAttemptsDoNotBlock(t *testing.T) {
	old := authlimit.Default
	authlimit.Default = authlimit.New(3, time.Minute, time.Minute)
	defer func() { authlimit.Default = old }()
	const weakKey = "short-key-12"
	srv := httptest.NewServer(NewREST(download.NewManager(download.Config{}), weakKey, "x"))
	defer srv.Close()
	get := func(key string, hdr map[string]string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/version?apikey="+key, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	foreign := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://attacker.example"},
		{"Referer": "https://attacker.example/page.html"},
		{"Origin": "null"},
	}
	if c := get(weakKey, foreign[0]); c != 200 {
		t.Fatalf("the right key from a link on another site was refused: %d", c)
	}
	for i := 0; i < 10; i++ {
		get("x", foreign[i%len(foreign)])
	}
	if c := get(weakKey, nil); c != 200 {
		t.Fatalf("cross-site attempts blocked the address: %d", c)
	}
	if c := get(weakKey, foreign[0]); c == 200 {
		t.Fatal("cross-site guessing of a short key is not limited")
	}
	if c := get(weakKey, map[string]string{"Sec-Fetch-Site": "same-origin"}); c != 200 {
		t.Fatalf("same-origin request refused: %d", c)
	}
}

// A reverse proxy with its own password sends "Authorization: Basic ...";
// that is not an API key and must not count as a wrong one.
func TestRESTBasicAuthIsNotAKey(t *testing.T) {
	old := authlimit.Default
	authlimit.Default = authlimit.New(2, time.Minute, time.Minute)
	defer func() { authlimit.Default = old }()
	const weakKey = "short-key-12"
	srv := httptest.NewServer(NewREST(download.NewManager(download.Config{}), weakKey, "x"))
	defer srv.Close()
	get := func(hdr map[string]string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/version", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 5; i++ {
		get(map[string]string{"Authorization": "Basic dXNlcjpwYXNz"})
	}
	if c := get(map[string]string{"X-Api-Key": weakKey}); c != 200 {
		t.Fatalf("Basic auth headers blocked the address: %d", c)
	}
	if c := get(map[string]string{"Authorization": "bearer " + weakKey}); c != 200 {
		t.Fatalf("lower-case bearer refused: %d", c)
	}
}

// The status carries the all-time totals when they are kept, and none
// otherwise.
func TestRESTStatusTotals(t *testing.T) {
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := download.NewManager(download.Config{Totals: func() stats.Totals {
		return stats.Totals{Uploaded: 123456789, Downloaded: 987654321, Since: since}
	}})
	srv := httptest.NewServer(NewREST(m, restKey, "x"))
	defer srv.Close()
	get := func(srv *httptest.Server) map[string]any {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/status", nil)
		req.Header.Set("X-Api-Key", restKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]any
		json.NewDecoder(resp.Body).Decode(&v)
		return v
	}
	tot, ok := get(srv)["totals"].(map[string]any)
	if !ok || tot["uploaded"] != float64(123456789) || tot["downloaded"] != float64(987654321) || tot["since"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("totals %v", tot)
	}
	none := httptest.NewServer(NewREST(download.NewManager(download.Config{}), restKey, "x"))
	defer none.Close()
	if _, ok := get(none)["totals"]; ok {
		t.Fatal("totals without a store")
	}
}

// A download's priority and preview are set and shown.
func TestRESTPriority(t *testing.T) {
	srv, _ := newTestREST(t)
	link := "ed2k://|file|Example.Film.mkv|20000000|0123456789ABCDEF0123456789ABCDEF|/"
	if code := call(t, srv, "POST", "/api/v1/downloads", "application/json", `{"links":["`+link+`"]}`, nil); code != 200 {
		t.Fatalf("add: %d", code)
	}
	path := "/api/v1/downloads/0123456789ABCDEF0123456789ABCDEF"
	if code := call(t, srv, "POST", path+"/priority", "application/json", `{"priority":"high"}`, nil); code != 200 {
		t.Fatalf("priority: %d", code)
	}
	if code := call(t, srv, "POST", path+"/priority", "application/json", `{"priority":"urgent"}`, nil); code != 400 {
		t.Fatalf("bad priority: %d", code)
	}
	if code := call(t, srv, "POST", path+"/preview", "application/json", `{"preview":true}`, nil); code != 200 {
		t.Fatalf("preview: %d", code)
	}
	var d RDownload
	call(t, srv, "GET", path, "", "", &d)
	if d.Priority != "high" || !d.Preview {
		t.Fatalf("%+v", d)
	}
}

// A collection (here as text) adds all its files.
func TestRESTCollection(t *testing.T) {
	srv, _ := newTestREST(t)
	text := "ed2k://|file|Example.Film.mkv|20000000|0123456789ABCDEF0123456789ABCDEF|/\n" +
		"ed2k://|file|Example.Show.S01E01.mkv|30000000|AAAA456789ABCDEF0123456789ABCDEF|/\n"
	var r AddResponse
	if code := call(t, srv, "POST", "/api/v1/downloads/collection", "application/octet-stream", text, &r); code != 200 || len(r.Added) != 2 {
		t.Fatalf("%d %+v", code, r)
	}
	if code := call(t, srv, "POST", "/api/v1/downloads/collection", "application/octet-stream", "nothing here", nil); code != 400 {
		t.Fatalf("not a collection: %d", code)
	}
}

// The one-time login link is given only with the API key.
func TestStartLinkEndpoint(t *testing.T) {
	m := download.NewManager(download.Config{TempDir: t.TempDir()})
	rest := NewREST(m, restKey, "test")
	rest.EnableStartLink(func() string { return "/web/start?t=abc" })
	srv := httptest.NewServer(rest)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/v1/web/link", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without the key: %d", resp.StatusCode)
	}
	var got map[string]string
	if code := call(t, srv, "POST", "/api/v1/web/link", "", "", &got); code != 200 || got["path"] != "/web/start?t=abc" {
		t.Fatalf("%d %v", code, got)
	}
}
