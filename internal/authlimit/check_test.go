package authlimit

import (
	"testing"
	"time"
)

func TestCheckStrongKeyNeverLockedOut(t *testing.T) {
	strong, weak := "0123456789abcdef0123456789abcdef", "weak-key-123"
	l := New(2, time.Minute, time.Hour)
	a := from("198.51.100.7:5000")
	l.Check(a, "x", weak)
	l.Check(a, "y", weak)
	if l.Allowed(a) {
		t.Fatal("not blocked after two wrong keys")
	}
	if l.Check(a, weak, weak) {
		t.Fatal("right short key accepted while blocked")
	}
	if !l.Check(a, strong, strong) {
		t.Fatal("right long key refused while blocked")
	}
	if l.Check(a, "nope", strong) || l.Check(a, "", "") {
		t.Fatal("wrong or empty key accepted")
	}
}

func TestCrossSite(t *testing.T) {
	cases := []struct {
		hdr  map[string]string
		want bool
	}{
		{nil, false}, // curl, the *arr apps
		{map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://elsewhere"}, false},
		{map[string]string{"Sec-Fetch-Site": "none"}, false}, // typed in the address bar
		{map[string]string{"Origin": "http://example.com"}, false},
		{map[string]string{"Origin": "http://EXAMPLE.com"}, false},
		{map[string]string{"Origin": "http://attacker.example"}, true},
		{map[string]string{"Origin": "null"}, true},
		{map[string]string{"Referer": "http://attacker.example/x"}, true},
		{map[string]string{"Referer": "http://example.com/ui"}, false},
	}
	for _, c := range cases {
		r := from("198.51.100.7:5000") // httptest: Host is example.com
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := CrossSite(r); got != c.want {
			t.Errorf("CrossSite(%v) = %v, want %v", c.hdr, got, c.want)
		}
	}
}
