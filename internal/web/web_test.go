package web

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mymule/internal/authlimit"
)

const testKey = "web-test-key-0123456789"

func newServer(t *testing.T) (*UI, *httptest.Server) {
	t.Helper()
	ui := New(testKey, "test")
	mux := http.NewServeMux()
	ui.Routes(mux)
	// Stand-in for the REST API: accepts the session like the real one.
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !ui.Session(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("/img", ui.RequireLogin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("image"))
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ui, srv
}

func do(t *testing.T, c *http.Client, method, url, body string, ui bool) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if ui {
		req.Header.Set("X-MyMule-UI", "1")
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestPagesAndHeaders(t *testing.T) {
	_, srv := newServer(t)
	resp, body := do(t, http.DefaultClient, "GET", srv.URL+"/", "", false)
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>MyMule</title>") {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP: %q", csp)
	}
	for _, p := range []string{"/assets/app.js", "/assets/terms.js", "/assets/app.css", "/assets/icon.svg"} {
		if resp, _ := do(t, http.DefaultClient, "GET", srv.URL+p, "", false); resp.StatusCode != 200 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := do(t, http.DefaultClient, "GET", srv.URL+"/assets/", "", false); resp.StatusCode != 404 {
		t.Errorf("directory listing: %d", resp.StatusCode)
	}
}

func TestLoginAndSession(t *testing.T) {
	ui, srv := newServer(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	if resp, _ := do(t, c, "GET", srv.URL+"/api/v1/status", "", true); resp.StatusCode != 401 {
		t.Fatalf("before login: %d", resp.StatusCode)
	}
	if resp, _ := do(t, c, "POST", srv.URL+"/web/login", `{"key":"wrong-key-000000000"}`, true); resp.StatusCode != 401 {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
	// Login without the UI header (e.g. a form on another site) is refused.
	if resp, _ := do(t, c, "POST", srv.URL+"/web/login", `{"key":"`+testKey+`"}`, false); resp.StatusCode != 403 {
		t.Fatalf("login without header: %d", resp.StatusCode)
	}
	resp, _ := do(t, c, "POST", srv.URL+"/web/login", `{"key":"`+testKey+`"}`, true)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	ck := resp.Cookies()[0]
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || strings.Contains(ck.Value, testKey) {
		t.Fatalf("cookie: %+v", ck)
	}
	if resp, _ := do(t, c, "GET", srv.URL+"/api/v1/status", "", true); resp.StatusCode != 200 {
		t.Fatalf("after login: %d", resp.StatusCode)
	}
	// Images (<img>) need the session cookie but not the header.
	if resp, _ := do(t, c, "GET", srv.URL+"/img", "", false); resp.StatusCode != 200 {
		t.Fatalf("image with session: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.DefaultClient, "GET", srv.URL+"/img", "", false); resp.StatusCode != 401 {
		t.Fatalf("image without session: %d", resp.StatusCode)
	}
	// The cookie alone is not enough (CSRF): the header is required.
	if resp, _ := do(t, c, "GET", srv.URL+"/api/v1/status", "", false); resp.StatusCode != 401 {
		t.Fatalf("cookie without header: %d", resp.StatusCode)
	}

	// Sessions expire, and a different api_key invalidates them.
	ui.now = func() time.Time { return time.Now().Add(sessionTTL + time.Hour) }
	if resp, _ := do(t, c, "GET", srv.URL+"/api/v1/status", "", true); resp.StatusCode != 401 {
		t.Fatalf("expired session: %d", resp.StatusCode)
	}
	ui.now = time.Now
	ui.key = "another-key-0123456789"
	if resp, _ := do(t, c, "GET", srv.URL+"/api/v1/status", "", true); resp.StatusCode != 401 {
		t.Fatalf("session after key change: %d", resp.StatusCode)
	}
	ui.key = testKey

	do(t, c, "POST", srv.URL+"/web/logout", "", true)
	if resp, _ := do(t, c, "GET", srv.URL+"/api/v1/status", "", true); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

func TestForgedSession(t *testing.T) {
	ui, _ := newServer(t)
	for _, v := range []string{"", "x", "9999999999.abc", "9999999999." + strings.Repeat("0", 64)} {
		r := httptest.NewRequest("GET", "/api/v1/status", nil)
		r.Header.Set("X-MyMule-UI", "1")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: v})
		if ui.Session(r) {
			t.Errorf("forged session %q accepted", v)
		}
	}
}

func TestLoginGuessingBlocked(t *testing.T) {
	old := authlimit.Default
	authlimit.Default = authlimit.New(3, time.Minute, time.Minute)
	defer func() { authlimit.Default = old }()
	_, srv := newServer(t)
	c := srv.Client()
	for i := 0; i < 3; i++ {
		if resp, _ := do(t, c, "POST", srv.URL+"/web/login", `{"key":"guess"}`, true); resp.StatusCode != 401 {
			t.Fatalf("wrong key %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := do(t, c, "POST", srv.URL+"/web/login", `{"key":"`+testKey+`"}`, true)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("right key while blocked: %d, want 429", resp.StatusCode)
	}
}

// Logging out ends the session on the server: a copy of the cookie stops
// working too, also after a restart. Logging out needs the UI header.
func TestLogoutRevokesSession(t *testing.T) {
	state := filepath.Join(t.TempDir(), "web_sessions.json")
	ui, srv := newServer(t)
	ui.SetStateFile(state)
	resp, _ := do(t, srv.Client(), "POST", srv.URL+"/web/login", `{"key":"`+testKey+`"}`, true)
	ck := resp.Cookies()[0]
	withCookie := func(method, path string, header bool) int {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.AddCookie(ck)
		if header {
			req.Header.Set("X-MyMule-UI", "1")
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := withCookie("GET", "/api/v1/status", true); c != 200 {
		t.Fatalf("session: %d", c)
	}
	if c := withCookie("POST", "/web/logout", false); c != 403 {
		t.Fatalf("logout without the UI header: %d", c)
	}
	if c := withCookie("GET", "/api/v1/status", true); c != 200 {
		t.Fatalf("session ended by a logout without header: %d", c)
	}
	withCookie("POST", "/web/logout", true)
	if c := withCookie("GET", "/api/v1/status", true); c != 401 {
		t.Fatalf("copied cookie after logout: %d", c)
	}
	// Another process (a restart) reading the same state file.
	again := New(testKey, "test")
	again.SetStateFile(state)
	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.AddCookie(ck)
	req.Header.Set("X-MyMule-UI", "1")
	if again.Session(req) {
		t.Fatal("logged-out session valid again after a restart")
	}
}

// Behind a reverse proxy that serves HTTPS the cookie is marked Secure.
func TestSecureCookieBehindProxy(t *testing.T) {
	_, srv := newServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/web/login", strings.NewReader(`{"key":"`+testKey+`"}`))
	req.Header.Set("X-MyMule-UI", "1")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ck := resp.Cookies(); len(ck) != 1 || !ck[0].Secure {
		t.Fatalf("cookie behind an HTTPS proxy: %+v", ck)
	}
}
