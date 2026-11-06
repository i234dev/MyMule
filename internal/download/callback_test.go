package download

import (
	"context"
	"net"
	"testing"
	"time"
)

// pub returns distinct public addresses for the tests. Nothing is dialled.
func pub(i int) net.IP { return net.IPv4(11, 0, byte(i>>8), byte(i)) }

func TestCallbackGateAddressAndState(t *testing.T) {
	var g callbackGate
	now := time.Now()
	if ok, _ := g.admit(pub(1), 4662, true, now); ok {
		t.Error("callback admitted while we are HighID")
	}
	for _, s := range []string{"127.0.0.1", "192.168.1.1", "10.0.0.5", "172.16.3.4", "100.64.0.1", "169.254.0.1", "224.0.0.1", "0.0.0.0"} {
		if ok, _ := g.admit(net.ParseIP(s), 4662, false, now); ok {
			t.Errorf("callback to %s admitted", s)
		}
	}
	if ok, _ := g.admit(pub(1), 0, false, now); ok {
		t.Error("callback to port 0 admitted")
	}
	if ok, why := g.admit(pub(1), 4662, false, now); !ok {
		t.Fatalf("legitimate callback refused: %s", why)
	}
	g.done()
	if ok, _ := g.admit(pub(1), 4662, false, now.Add(10*time.Second)); ok {
		t.Error("same address called back again within a minute")
	}
	if ok, _ := g.admit(pub(1), 4662, false, now.Add(callbackRepeatAfter)); !ok {
		t.Error("same address refused after the repeat interval")
	}
}

func TestCallbackGateLimits(t *testing.T) {
	var g callbackGate
	now := time.Now()
	for i := 0; i < maxCallbacksActive; i++ {
		if ok, why := g.admit(pub(i), 4662, false, now); !ok {
			t.Fatalf("callback %d refused: %s", i+1, why)
		}
	}
	if ok, _ := g.admit(pub(100), 4662, false, now); ok {
		t.Fatal("callback admitted over the in-progress limit")
	}
	for i := 0; i < maxCallbacksActive; i++ {
		g.done()
	}
	// The rest of the per-minute budget, each finishing at once.
	for i := maxCallbacksActive; i < maxCallbacksPerMin; i++ {
		if ok, why := g.admit(pub(i), 4662, false, now); !ok {
			t.Fatalf("callback %d refused: %s", i+1, why)
		}
		g.done()
	}
	if ok, _ := g.admit(pub(200), 4662, false, now.Add(30*time.Second)); ok {
		t.Fatal("callback admitted over the per-minute limit")
	}
	if ok, why := g.admit(pub(200), 4662, false, now.Add(61*time.Second)); !ok {
		t.Fatalf("callback refused after the minute passed: %s", why)
	}
}

// A server asking us to call back an address on our own machine must
// not make us connect (it used to, unconditionally).
func TestOnCallbackDoesNotDialLocalAddresses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
			accepted <- struct{}{}
		}
	}()
	m := NewManager(Config{})
	a := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.onCallback(ctx, a.IP, uint16(a.Port))
	select {
	case <-accepted:
		t.Fatalf("callback to %s connected", a)
	case <-time.After(300 * time.Millisecond):
	}
}
