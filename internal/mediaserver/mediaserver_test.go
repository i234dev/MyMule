package mediaserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	if err := Check("", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := Check("jellyfin", "http://192.0.2.5:8096", "example-token"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{{"kodi", "http://x", "t"}, {"plex", "ftp://x", "t"}, {"plex", "http://x", ""}} {
		if Check(c[0], c[1], c[2]) == nil {
			t.Errorf("%v accepted", c)
		}
	}
}

// Each server is asked its own way; scans asked close together make one.
func TestRefresh(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Plex-Token")+r.Header.Get("X-Emby-Token"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	plex, _ := New("plex", srv.URL+"/", "plex-token", nil)
	if err := plex.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	jf, _ := New("jellyfin", srv.URL, "jf-token", nil)
	jf.Gap = 50 * time.Millisecond
	for range 5 {
		jf.Refresh()
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"GET /library/sections/all/refresh plex-token", "POST /Library/Refresh jf-token"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("got %q", seen)
	}
	var none *Refresher
	none.Refresh()
	none.Stop()
	if r, err := New("", "", "", nil); r != nil || err != nil {
		t.Fatal("empty kind")
	}
}
