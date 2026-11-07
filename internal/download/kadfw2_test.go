package download

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// A node we serve that stops reading cannot block whoever sends to it.
func TestBuddySendNeverBlocks(t *testing.T) {
	sends := make(chan func(byte, []byte) bool, 1)
	m := NewManager(Config{
		KadIncomingBuddy:   func(ip net.IP, user ed2k.Hash, s func(byte, []byte) bool) bool { sends <- s; return true },
		KadServedBuddyGone: func(net.IP) {},
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			time.Sleep(5 * time.Second) // never reads
			nc.Close()
		}
	}()
	nc, _ := ln.Accept()
	c := peer.NewConn(nc)
	ctx, cancel := context.WithCancel(context.Background())
	go m.incomingBuddy(ctx, c, peer.Hello{})
	send := <-sends
	big := make([]byte, 60000)
	start := time.Now()
	refused := 0
	for range 2000 {
		if !send(kad.OpReaskCallbackTCP, big) {
			refused++
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("sending took %v", d)
	}
	if refused == 0 {
		t.Fatal("nothing dropped from a full queue")
	}
	cancel()
	c.Close()
}

// Callbacks through Kad go through the callback gate.
func TestKadConnectToGate(t *testing.T) {
	m := kadManager(t)
	ip := net.ParseIP("11.1.1.1")
	if !m.KadConnectTo(ip, 1) {
		t.Fatal("first refused")
	}
	if m.KadConnectTo(ip, 1) {
		t.Fatal("same address twice within a minute")
	}
	if m.KadConnectTo(net.ParseIP("192.168.1.9"), 1) {
		t.Fatal("private address accepted")
	}
	ok := 0
	for i := range 60 {
		if m.KadConnectTo(net.IPv4(11, 1, 2, byte(i)), 1) {
			ok++
		}
	}
	if ok >= maxCallbacksPerMin {
		t.Fatalf("%d callbacks in a minute", ok)
	}
}

// Type 6 sources without the direct callback option are not used (eMule
// drops them too); queued firewalled sources connect at their turn.
func TestKadSourceRules(t *testing.T) {
	m := NewManager(Config{KadCallback: func(kad.Source, ed2k.Hash) error { return nil }})
	f := kadFile(m, 1)
	m.onKadSource(f.pf.Hash, kad.Source{UserHash: [16]byte{1}, Type: 6, IP: netip.MustParseAddr("11.2.3.4"), UDPPort: 4672})
	if len(f.sources) != 0 {
		t.Fatal("type 6 without the option used")
	}
	m.onKadSource(f.pf.Hash, kad.Source{UserHash: [16]byte{2}, Type: 6, IP: netip.MustParseAddr("11.2.3.4"), UDPPort: 4672, Crypt: kad.ConnectDirectCallback})
	src := f.sources[kadSourceKey([16]byte{2})]
	if src == nil {
		t.Fatal("type 6 with the option not used")
	}
	src.State = StateQueued
	if !incomingMatches(src, peer.Hello{UserHash: ed2k.Hash{2}}, net.IPv4(11, 2, 3, 4), time.Now()) {
		t.Fatal("queued source not recognised at its turn")
	}
}

// The client we offered to be the buddy of learns our Kad port as our UDP
// port; others do not.
func TestBuddyHelloUDPPort(t *testing.T) {
	m := NewManager(Config{UserHash: ed2k.Hash{3}, Kad: func() (uint16, byte) { return 4672, 8 },
		KadBuddyOffer: func(ip net.IP, user ed2k.Hash) bool { return user == ed2k.Hash{7} }})
	for _, tc := range []struct {
		user ed2k.Hash
		want uint16
	}{{ed2k.Hash{7}, 4672}, {ed2k.Hash{8}, 0}} {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		got := make(chan peer.Hello, 1)
		go func() {
			nc, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				return
			}
			c := peer.NewConn(nc)
			defer c.Close()
			c.Send(proto.ProtoEDonkey, peer.OpHello, peer.NewHello(tc.user, "x", 0x05060708, 1, nil, 0).Encode(false))
			p, err := c.Read(5 * time.Second)
			if err == nil {
				h, _ := peer.DecodeHello(p.Payload, true)
				got <- h
			}
		}()
		nc, _ := ln.Accept()
		c := peer.NewConn(nc)
		m.handshake(c, false)
		h := <-got
		c.Close()
		ln.Close()
		if udp := uint16(h.Tags.Int(peer.CTEMuleUDPPorts)); udp != tc.want || h.KadPort() != 4672 {
			t.Fatalf("user %x: UDP port %d, Kad port %d", tc.user[0], udp, h.KadPort())
		}
	}
}
