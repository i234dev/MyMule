package netutil

import (
	"net"
	"testing"
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
