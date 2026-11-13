package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A start link logs the browser in once, within its time, and only from
// this computer; what it leaves is a session cookie and a redirect to the
// interface, never the token in the address.
func TestStartLink(t *testing.T) {
	u := New("example-key-for-tests-0123456789", "test")
	now := time.Now()
	u.now = func() time.Time { return now }
	mux := http.NewServeMux()
	u.Routes(mux)
	open := func(path, from string, proxied bool) (*httptest.ResponseRecorder, bool) {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = from
		if proxied {
			r.Header.Set("X-Forwarded-For", "203.0.113.5")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		got := false
		for _, c := range w.Result().Cookies() {
			got = got || (c.Name == cookieName && c.Value != "")
		}
		return w, got
	}

	link := u.StartLink()
	w, session := open(link, "127.0.0.1:50000", false)
	if !session || w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("link: %d to %q, session %v", w.Code, w.Header().Get("Location"), session)
	}
	if _, again := open(link, "127.0.0.1:50000", false); again {
		t.Fatal("a link worked twice")
	}

	for _, tc := range []struct {
		name, from string
		proxied    bool
	}{{"from another computer", "192.0.2.7:50000", false}, {"through a proxy", "127.0.0.1:50000", true}} {
		if _, got := open(u.StartLink(), tc.from, tc.proxied); got {
			t.Errorf("%s: logged in", tc.name)
		}
	}

	late := u.StartLink()
	now = now.Add(linkTTL + time.Second)
	if _, got := open(late, "127.0.0.1:50000", false); got {
		t.Fatal("an expired link worked")
	}
	if _, got := open("/web/start?t="+strings.Repeat("ab", 32), "[::1]:50000", false); got {
		t.Fatal("a made-up token worked")
	}
	for range maxLinks + 5 {
		u.StartLink()
	}
	if len(u.links) > maxLinks {
		t.Fatalf("%d links kept", len(u.links))
	}
}
