package download

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"mymule/internal/ipfilter"
	"mymule/internal/server"
)

func holderOf(t *testing.T, list string) *ipfilter.Holder {
	t.Helper()
	f, _, err := ipfilter.Parse(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	h := &ipfilter.Holder{}
	h.Set(f)
	return h
}

// Blocked addresses are not taken as sources, by hand or from a server,
// are not dialed, and blocked servers are not asked.
func TestIPFilterSourcesAndDials(t *testing.T) {
	m, f := sxManager(t, nil)
	m.cfg.IPFilter = holderOf(t, "198.51.100.0 - 198.51.100.255 , 0 , test\n")
	m.cfg.allowPrivateSources = true

	if err := m.AddSource(f.pf.Hash, "198.51.100.7:4662"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked source added by hand: %v", err)
	}
	if err := m.AddSource(f.pf.Hash, "203.0.113.7:4662"); err != nil {
		t.Fatal(err)
	}
	n := m.addSources(f, []server.Source{
		{ID: server.IPToU32(net.ParseIP("198.51.100.8")), Port: 4662},
		{ID: server.IPToU32(net.ParseIP("203.0.113.8")), Port: 4662},
	}, "test", 0, false)
	if n != 1 {
		t.Fatalf("%d sources added", n)
	}
	for key := range f.sources {
		if strings.HasPrefix(key, "198.51.100.") {
			t.Fatalf("blocked source %s taken", key)
		}
	}

	if _, err := m.dial(context.Background(), "tcp", "198.51.100.9:4662", [16]byte{}, 0); !errors.Is(err, errBlocked) {
		t.Fatalf("blocked address dialed: %v", err)
	}

	m.cfg.Servers = func() []server.Info {
		return []server.Info{{IP: net.ParseIP("198.51.100.1"), Port: 4661}, {IP: net.ParseIP("203.0.113.1"), Port: 4661}}
	}
	if s := m.servers(); len(s) != 1 || !s[0].IP.Equal(net.ParseIP("203.0.113.1")) {
		t.Fatalf("servers %v", s)
	}
	// No filter: nothing blocked.
	m.cfg.IPFilter = nil
	if len(m.servers()) != 2 {
		t.Fatal("nil filter blocks")
	}
}

// A connection from a blocked address is closed at once.
func TestIPFilterIncoming(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(Config{IPFilter: holderOf(t, "127.0.0.1 - 127.0.0.1 , 0 , test\n")})
	ctx, cancel := context.WithCancel(context.Background())
	m.wg.Add(1)
	go m.acceptLoop(ctx, ln)
	defer func() { cancel(); ln.Close(); m.wg.Wait() }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || !isReset(err) {
		t.Fatalf("connection from a blocked address kept: %v", err)
	}
}

// The server connection refuses a blocked server.
func TestIPFilterServerDial(t *testing.T) {
	h := holderOf(t, "198.51.100.0 - 198.51.100.255 , 0 , test\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := server.Dial(ctx, server.Info{IP: net.ParseIP("198.51.100.1"), Port: 4661}, server.Config{Blocked: h.BlockedIP})
	if err == nil || !strings.Contains(err.Error(), "IP filter") {
		t.Fatalf("blocked server: %v", err)
	}
}
