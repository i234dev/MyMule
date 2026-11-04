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

	"mymule/internal/download"
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
