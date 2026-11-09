package netutil

import (
	"net"
	"testing"
	"time"
)

func TestIsPublicIPv4(t *testing.T) {
	for _, s := range []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "100.127.255.254", "127.0.0.1",
		"169.254.1.1", "172.16.0.1", "172.31.255.255", "192.0.0.8", "192.0.2.10",
		"192.168.1.1", "198.18.0.1", "198.51.100.20", "203.0.113.5",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255", "::1", "2001:db8::1",
	} {
		if IsPublicIPv4(net.ParseIP(s)) {
			t.Errorf("%s reported as public", s)
		}
	}
	if IsPublicIPv4(nil) {
		t.Error("nil reported as public")
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.1", "172.15.255.255", "172.32.0.1", "223.255.255.254"} {
		if !IsPublicIPv4(net.ParseIP(s)) {
			t.Errorf("%s reported as not public", s)
		}
	}
}

// The limiter spreads bytes to its rate and says how long it waited;
// unlimited (or nil) it never waits.
func TestLimiter(t *testing.T) {
	l := NewLimiter(100 << 10) // 100 KB/s, the bucket full
	start := time.Now()
	var waited time.Duration
	for range 30 {
		waited += l.Wait(10 << 10)
	}
	el := time.Since(start)
	// 300 KB at 100 KB/s, the first 100 KB at once: about 2 s.
	if el < 1500*time.Millisecond || el > 4*time.Second || waited < time.Second {
		t.Fatalf("took %v, waited %v", el, waited)
	}
	if NewLimiter(0).Wait(1<<20) != 0 || (*Limiter)(nil).Wait(1<<20) != 0 {
		t.Fatal("unlimited waited")
	}
}
