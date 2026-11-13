package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1.3.6", "1.3.5", true}, {"v1.4.0", "1.3.9", true}, {"2.0.0", "v1.99.99", true}, {"1.10.0", "1.9.0", true},
		{"1.3.5", "1.3.5", false}, {"1.3.4", "1.3.5", false}, {"1.3.6", "dev", false}, {"x", "1.0.0", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// A newer release is found and told once; what GitHub sends is checked.
func TestCheck(t *testing.T) {
	answer := `{"tag_name":"v1.4.0","html_url":"https://github.com/i234dev/MyMule/releases/tag/v1.4.0","published_at":"2026-11-20T10:00:00Z"}`
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		w.Write([]byte(answer))
	}))
	defer srv.Close()
	var logged []string
	c := &Checker{Current: "1.3.5", URL: srv.URL, UserAgent: "MyMule/1.3.5", Client: srv.Client(),
		Log: func(f string, a ...any) { logged = append(logged, f) }}
	if c.Newer() != nil {
		t.Fatal("newer before asking")
	}
	for range 2 {
		if err := c.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if r := c.Newer(); r == nil || r.Version != "1.4.0" || !strings.HasSuffix(r.URL, "/tag/v1.4.0") {
		t.Fatalf("newer: %+v", r)
	}
	if len(logged) != 1 || agent != "MyMule/1.3.5" {
		t.Fatalf("logged %d times, agent %q", len(logged), agent)
	}

	// The same version: nothing to tell.
	c.Current = "v1.4.0"
	c.Check(context.Background())
	if c.Newer() != nil {
		t.Fatal("newer than itself")
	}

	// A page elsewhere is not linked; a strange version is refused.
	c.Current = "1.3.5"
	answer = `{"tag_name":"v1.4.1","html_url":"https://evil.example/download"}`
	c.Check(context.Background())
	if r := c.Newer(); r == nil || r.URL != releasesPage+"latest" {
		t.Fatalf("link elsewhere: %+v", r)
	}
	for _, bad := range []string{`{"tag_name":"v9.9.9-beta","html_url":""}`, `{"tag_name":"<script>","html_url":""}`,
		`{"tag_name":"v9.9.9","prerelease":true}`, `not json`} {
		answer = bad
		if err := c.Check(context.Background()); err == nil {
			t.Errorf("%s: taken", bad)
		}
	}
}
