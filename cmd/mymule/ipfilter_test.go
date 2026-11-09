package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The list is downloaded when there is none, kept as it was when a
// download is not a list, read again when the file changes, and dropped
// when the file goes.
func TestIPFilterFile(t *testing.T) {
	body := "Example:198.51.100.0-198.51.100.255\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	dir := t.TempDir()
	f := openIPFilter(dir, srv.URL, t.Logf)
	if f.holder.Get() != nil {
		t.Fatal("filter without a file")
	}
	now := time.Now()
	if !f.due(now) {
		t.Fatal("no list, and not due")
	}
	if err := fetchIPFilter(context.Background(), f.url, f.path); err != nil {
		t.Fatal(err)
	}
	f.reload()
	if !f.holder.Blocked(netip.MustParseAddr("198.51.100.5")) {
		t.Fatal("downloaded list not used")
	}
	if f.due(now) {
		t.Fatal("fresh list due again")
	}

	// Not a list: the file stays as it was.
	body = "<html>not found</html>\n"
	if err := fetchIPFilter(context.Background(), f.url, f.path); err == nil {
		t.Fatal("a page that is not a list taken")
	}
	f.reload()
	if !f.holder.Blocked(netip.MustParseAddr("198.51.100.5")) {
		t.Fatal("bad download replaced the list")
	}

	// Changed by hand: read again.
	os.WriteFile(f.path, []byte("203.0.113.0 - 203.0.113.255 , 0 , by hand, longer line\n"), 0o644)
	f.reload()
	if f.holder.Blocked(netip.MustParseAddr("198.51.100.5")) || !f.holder.Blocked(netip.MustParseAddr("203.0.113.5")) {
		t.Fatal("changed file not read again")
	}
	// Old: due again, but not within ipFilterRetry of a try.
	old := time.Now().Add(-ipFilterMaxAge - time.Hour)
	os.Chtimes(f.path, old, old)
	if !f.due(time.Now()) {
		t.Fatal("old list not due")
	}
	f.lastTry = time.Now()
	if f.due(time.Now()) {
		t.Fatal("tried again too soon")
	}
	// No URL: never downloaded.
	if (&ipFilter{path: filepath.Join(dir, "none")}).due(time.Now()) {
		t.Fatal("due without a URL")
	}

	os.Remove(f.path)
	f.reload()
	if f.holder.Get() != nil {
		t.Fatal("removed file still used")
	}
}
