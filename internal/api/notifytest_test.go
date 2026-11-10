package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mymule/internal/download"
	"mymule/internal/mediaserver"
	"mymule/internal/notify"
)

// The test button sends a notification and asks for a scan, and says how
// each went.
func TestNotifyTestEndpoint(t *testing.T) {
	var mu sync.Mutex
	var got []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	n, err := notify.New(hook.URL+"/hook", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := mediaserver.New("jellyfin", hook.URL, "example-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	rest := NewREST(download.NewManager(download.Config{TempDir: t.TempDir()}), restKey, "test")
	rest.EnableNotifyTest(n, ms)
	srv := httptest.NewServer(rest)
	defer srv.Close()
	var out map[string]string
	if code := call(t, srv, "POST", "/api/v1/notify/test", "", "", &out); code != 200 || out["notify"] != "sent" || out["media_server"] != "scanning" {
		t.Fatalf("%d %v", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !strings.Contains(got[0], `"event":"test"`) || got[1] != "POST /Library/Refresh " {
		t.Fatalf("%q", got)
	}

	off := NewREST(download.NewManager(download.Config{TempDir: t.TempDir()}), restKey, "test")
	off.EnableNotifyTest(nil, nil)
	srv2 := httptest.NewServer(off)
	defer srv2.Close()
	if call(t, srv2, "POST", "/api/v1/notify/test", "", "", &out); out["notify"] != "off" || out["media_server"] != "off" {
		t.Fatalf("%v", out)
	}

	// Why it failed is not told: it would map the network.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	bad, _ := mediaserver.New("jellyfin", closed.URL, "example-token", nil)
	failing := NewREST(download.NewManager(download.Config{TempDir: t.TempDir()}), restKey, "test")
	var logged []string
	failing.logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	failing.EnableNotifyTest(nil, bad)
	srv3 := httptest.NewServer(failing)
	defer srv3.Close()
	if call(t, srv3, "POST", "/api/v1/notify/test", "", "", &out); out["media_server"] != "failed (see the MyMule log)" || len(logged) != 1 {
		t.Fatalf("%v %q", out, logged)
	}
}
