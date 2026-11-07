package download

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// A firewalled Kad source is reached by a callback, and known by its user
// hash when it connects.
func TestKadCallbackSource(t *testing.T) {
	var mu sync.Mutex
	var asked []kad.Source
	m := NewManager(Config{MaxConns: 10, KadCallback: func(s kad.Source, file ed2k.Hash) error {
		mu.Lock()
		asked = append(asked, s)
		mu.Unlock()
		return nil
	}})
	f := kadFile(m, 1)
	user := [16]byte{5, 5}
	s := kad.Source{UserHash: user, Type: 3, BuddyIP: netip.MustParseAddr("11.2.3.4"), BuddyPort: 4672}
	m.onKadSource(f.pf.Hash, s)
	m.onKadSource(f.pf.Hash, s) // twice: once
	if len(f.sources) != 1 {
		t.Fatalf("%d sources", len(f.sources))
	}
	src := f.sources[kadSourceKey(user)]
	m.schedule(context.Background())
	if len(asked) != 1 || asked[0].BuddyIP != s.BuddyIP || src.callbackAt.IsZero() {
		t.Fatalf("callback not asked: %v", asked)
	}
	now := time.Now()
	h := peer.Hello{UserHash: ed2k.Hash(user)}
	if !incomingMatches(src, h, net.IPv4(11, 9, 9, 9), now) {
		t.Fatal("source not recognised when it connects")
	}
	if incomingMatches(src, peer.Hello{UserHash: ed2k.Hash{9}}, net.IPv4(11, 9, 9, 9), now) {
		t.Fatal("another client taken for the source")
	}
	if incomingMatches(src, h, net.IPv4(11, 9, 9, 9), now.Add(callbackWait+time.Second)) {
		t.Fatal("recognised after the wait")
	}
	// Type 2 (not used by eMule) is not a source.
	m.onKadSource(f.pf.Hash, kad.Source{UserHash: [16]byte{6}, Type: 2})
	if len(f.sources) != 1 {
		t.Fatal("type 2 source added")
	}
}

// A peer asking for the UDP test reaches the Kad side.
func TestKadUDPCheckRequestPacket(t *testing.T) {
	var got []byte
	m := NewManager(Config{KadUDPCheckRequest: func(ip net.IP, p []byte) { got = p }})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		if nc, err := net.Dial("tcp", ln.Addr().String()); err == nil {
			time.Sleep(time.Second)
			nc.Close()
		}
	}()
	nc, _ := ln.Accept()
	c := peer.NewConn(nc)
	defer c.Close()
	if !m.kadPacket(c, &proto.Packet{Proto: proto.ProtoEMule, Opcode: kad.OpFWCheckUDPReq, Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8}}) || len(got) != 8 {
		t.Fatal("not passed on")
	}
}

// We ask a client for the UDP test: only clients that do it get the
// request; the others count as failed.
func TestKadUDPCheck(t *testing.T) {
	m := kadManager(t)
	port, got := fakeKadPeer(t, 8)
	failed := make(chan bool, 2)
	if !m.KadUDPCheck(net.ParseIP("127.0.0.1"), port, []byte{1, 2, 3, 4, 5, 6, 7, 8}, func() { failed <- true }) {
		t.Fatal("refused")
	}
	select {
	case p := <-got:
		if p == nil || p.Opcode != kad.OpFWCheckUDPReq || len(p.Payload) != 8 {
			t.Fatalf("got %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no request")
	}
	port, got = fakeKadPeer(t, 5)
	m.KadUDPCheck(net.ParseIP("127.0.0.1"), port, []byte{1}, func() { failed <- true })
	select {
	case <-failed:
	case <-time.After(10 * time.Second):
		t.Fatal("old client not reported as failed")
	}
	if p, ok := <-got; ok {
		t.Fatalf("request sent to an old client: %+v", p)
	}
}

// Serving as a buddy: pings get pongs, at most every 3 minutes; the Kad
// side's callbacks reach the peer.
func TestServeBuddy(t *testing.T) {
	sends := make(chan func(byte, []byte) bool, 2)
	gone := make(chan bool, 1)
	m := NewManager(Config{
		KadIncomingBuddy: func(ip net.IP, user ed2k.Hash, s func(byte, []byte) bool) bool {
			if user != (ed2k.Hash{4}) {
				return false
			}
			sends <- s
			return true
		},
		KadServedBuddyGone: func(net.IP) { gone <- true },
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	clientDone := make(chan []*proto.Packet, 1)
	go func() {
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		c := peer.NewConn(nc)
		defer c.Close()
		c.Send(proto.ProtoEMule, kad.OpBuddyPing, nil)
		c.Send(proto.ProtoEMule, kad.OpBuddyPing, nil) // too soon: no pong
		var got []*proto.Packet
		for {
			p, err := c.Read(2 * time.Second)
			if err != nil {
				break
			}
			got = append(got, p)
		}
		clientDone <- got
	}()
	nc, _ := ln.Accept()
	c := peer.NewConn(nc)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan bool, 1)
	go func() { served <- m.incomingBuddy(ctx, c, peer.Hello{UserHash: ed2k.Hash{4}}) }()
	send := <-sends
	time.Sleep(500 * time.Millisecond)
	send(kad.OpCallback, []byte{1, 2, 3})
	got := <-clientDone
	cancel()
	c.Close()
	if !<-served {
		t.Fatal("not served")
	}
	<-gone
	pongs, callbacks := 0, 0
	for _, p := range got {
		switch p.Opcode {
		case kad.OpBuddyPong:
			pongs++
		case kad.OpCallback:
			callbacks++
		}
	}
	if pongs != 1 || callbacks != 1 {
		t.Fatalf("%d pongs, %d callbacks", pongs, callbacks)
	}
	// Someone else: not served.
	if m.incomingBuddy(context.Background(), c, peer.Hello{UserHash: ed2k.Hash{5}}) {
		t.Fatal("another peer served")
	}
}

// Our buddy connection: up when the right client answers, callbacks make
// us connect to the downloader (for files we have), down when it ends.
func TestOurBuddy(t *testing.T) {
	downloader, _ := net.Listen("tcp", "127.0.0.1:0")
	defer downloader.Close()
	dport := uint16(downloader.Addr().(*net.TCPAddr).Port)
	connected := make(chan bool, 1)
	go func() {
		if nc, err := downloader.Accept(); err == nil {
			connected <- true
			nc.Close()
		}
	}()
	up := make(chan uint16, 1)
	down := make(chan bool, 1)
	m := NewManager(Config{UserHash: ed2k.Hash{3}, Nick: "me", Kad: func() (uint16, byte) { return 4672, 8 }, allowPrivateSources: true,
		KadBuddyUp:   func(ip net.IP, p uint16) { up <- p },
		KadBuddyDown: func(net.IP) { down <- true },
		KadOwnCallback: func(p []byte) (netip.Addr, uint16, [16]byte, bool) {
			return netip.MustParseAddr("127.0.0.1"), dport, [16]byte{1}, len(p) == 3
		}})
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	kadFile(m, 1) // we download file {1}
	// The buddy: answers the hello, then passes a callback.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		c := peer.NewConn(nc)
		defer c.Close()
		c.Read(5 * time.Second)
		h := peer.NewHello(ed2k.Hash{9}, "buddy", 0x05060708, 4662, nil, 0).WithKad(4672, 8)
		c.Send(proto.ProtoEDonkey, peer.OpHelloAnswer, h.Encode(true))
		if p, err := c.Read(5 * time.Second); err != nil || p.Opcode != kad.OpBuddyPing {
			return
		}
		c.Send(proto.ProtoEMule, kad.OpCallback, []byte{1, 2, 3})
		time.Sleep(2 * time.Second)
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	if !m.KadConnectBuddy(net.ParseIP("127.0.0.1"), port, ed2k.Hash{9}) {
		t.Fatal("refused")
	}
	if m.KadConnectBuddy(net.ParseIP("127.0.0.1"), port, ed2k.Hash{9}) {
		t.Fatal("two buddy connections at once")
	}
	select {
	case p := <-up:
		if p != 4672 {
			t.Fatalf("buddy Kad port %d", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never up")
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("did not connect to the downloader")
	}
	select {
	case <-down:
	case <-time.After(15 * time.Second):
		t.Fatal("never down")
	}
	cancel()
	m.wg.Wait()
	// The wrong client at the buddy's address: never up.
	m2 := NewManager(Config{UserHash: ed2k.Hash{3}, KadBuddyUp: func(net.IP, uint16) { up <- 0 }, KadBuddyDown: func(net.IP) {}})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m2.ctx = ctx2
	p2, _ := fakeKadPeer(t, 8) // its user hash is {9}
	m2.KadConnectBuddy(net.ParseIP("127.0.0.1"), p2, ed2k.Hash{8})
	select {
	case <-up:
		t.Fatal("up with another client")
	case <-time.After(3 * time.Second):
	}
}

// Our hello tells our buddy and direct callbacks.
func TestHelloKadFirewall(t *testing.T) {
	m := NewManager(Config{Kad: func() (uint16, byte) { return 4672, 8 },
		KadFirewall: func() KadFirewall {
			return KadFirewall{BuddyIP: net.IPv4(11, 2, 3, 4), BuddyPort: 4000, DirectCallback: true}
		}})
	h := m.ourHello()
	if h.Tags.Int(peer.CTEMuleBuddyUDP) != 4000 || h.Tags.Int(peer.CTEMuleMiscOpts2)&(1<<12) == 0 {
		t.Fatalf("tags %v", h.Tags)
	}
	ip := uint32(h.Tags.Int(peer.CTEMuleBuddyIP))
	if byte(ip) != 11 || byte(ip>>24) != 4 {
		t.Fatalf("buddy IP %08X (network order expected)", ip)
	}
}
