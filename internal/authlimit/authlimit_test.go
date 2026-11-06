package authlimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func from(addr string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = addr
	return r
}

func TestLimiterBlocksAfterTooManyFailures(t *testing.T) {
	now := time.Now()
	l := New(3, time.Minute, 5*time.Minute)
	l.now = func() time.Time { return now }
	a := from("198.51.100.7:5000")
	for i := 0; i < 2; i++ {
		l.Failed(a)
	}
	if !l.Allowed(a) {
		t.Fatal("blocked before the limit")
	}
	l.Failed(from("198.51.100.7:6000")) // same IP, another port
	if l.Allowed(a) {
		t.Fatal("not blocked after the limit")
	}
	if !l.Allowed(from("198.51.100.8:5000")) {
		t.Fatal("another address was blocked too")
	}
	now = now.Add(5*time.Minute + time.Second)
	if !l.Allowed(a) {
		t.Fatal("still blocked after the block time")
	}
}

func TestLimiterWindow(t *testing.T) {
	now := time.Now()
	l := New(3, time.Minute, 5*time.Minute)
	l.now = func() time.Time { return now }
	a := from("198.51.100.7:5000")
	l.Failed(a)
	l.Failed(a)
	now = now.Add(2 * time.Minute) // old failures expire
	l.Failed(a)
	if !l.Allowed(a) {
		t.Fatal("failures outside the window counted")
	}
}

// One IPv6 customer gets a whole /64: rotating addresses inside it must
// not reset the count.
func TestLimiterGroupsIPv6(t *testing.T) {
	l := New(2, time.Minute, time.Minute)
	l.Failed(from("[2001:db8:1:2::1]:5000"))
	l.Failed(from("[2001:db8:1:2::ffff]:5000"))
	if l.Allowed(from("[2001:db8:1:2::abcd]:5000")) {
		t.Fatal("same /64 not blocked")
	}
	if !l.Allowed(from("[2001:db8:1:3::1]:5000")) {
		t.Fatal("another /64 blocked")
	}
}

func TestHostCheck(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := HostCheck([]string{"mule.example.org"}, ok)
	for host, want := range map[string]int{
		"127.0.0.1:4711":         200,
		"192.0.2.10:4711":        200,
		"[::1]:4711":             200,
		"localhost:4711":         200,
		"LOCALHOST":              200,
		"app.localhost:4711":     200,
		"mule.example.org":       200,
		"Mule.Example.Org.:443":  200,
		"evil.example.com:4711":  421,
		"127.0.0.1.evil.example": 421,
		"":                       421,
	} {
		r := httptest.NewRequest("GET", "/api/v1/status", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("Host %q: %d, want %d", host, rec.Code, want)
		}
	}
}
