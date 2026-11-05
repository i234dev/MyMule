package web

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	for _, p := range []string{"/assets/app.js", "/assets/app.css", "/assets/icon.svg"} {
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
