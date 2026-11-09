package download

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/server"
)

// highIDServer is a server connection that gave us a HighID.
func highIDServer(t *testing.T) *server.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		proto.ReadPacket(c)
		var w proto.Writer
		w.Bytes([]byte{5, 6, 7, 8})
		c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpIDChange, Payload: w.B}).Encode())
		time.Sleep(10 * time.Second)
	}()
	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv, err := server.Dial(ctx, server.Info{IP: addr.IP, Port: uint16(addr.Port)}, server.Config{Nick: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

type sentUDP struct {
	to  netip.AddrPort
	op  byte
	pay []byte
}

// A queued source that offers client UDP is asked for our place by UDP,
// not by TCP; its answer, from the address and port asked, keeps the
// place and moves the next ask; no answer in time and TCP is used.
func TestUDPReask(t *testing.T) {
	m, f := sxManager(t, nil)
	var mu sync.Mutex
	var sent []sentUDP
	m.cfg.SendClientUDP = func(to netip.AddrPort, op byte, p []byte, _ ed2k.Hash) {
		mu.Lock()
		sent = append(sent, sentUDP{to, op, p})
		mu.Unlock()
	}
	m.cfg.MaxConns = 10
	m.srv = highIDServer(t)
	src := &Source{key: "198.51.100.5:4662", IP: net.ParseIP("198.51.100.5").To4(), Port: 4662, file: f,
		State: StateQueued, Rank: 40, udpPort: 4672, udpVer: 4, NextAsk: time.Now().Add(-time.Second)}
	f.sources[src.key] = src

	m.schedule(context.Background())
	m.mu.Lock()
	busy, asked := src.busy, !src.udpAskAt.IsZero()
	m.mu.Unlock()
	mu.Lock()
	n := len(sent)
	mu.Unlock()
	if busy || !asked || n != 1 || sent[0].op != peer.OpReaskFilePing || sent[0].to != netip.MustParseAddrPort("198.51.100.5:4672") {
		t.Fatalf("busy %v, asked %v, sent %+v", busy, asked, sent)
	}
	if file, err := peer.DecodeReaskFilePing(sent[0].pay, 4); err != nil || file != f.pf.Hash {
		t.Fatalf("ping: %v", err)
	}

	// An answer from another port is not taken; from the one asked, it is.
	ack := peer.EncodeReaskAck(4, nil, 7)
	m.OnClientUDP(netip.MustParseAddrPort("198.51.100.5:9999"), peer.OpReaskAck, ack)
	if src.Rank != 40 {
		t.Fatal("answer from another port taken")
	}
	m.OnClientUDP(netip.MustParseAddrPort("198.51.100.5:4672"), peer.OpReaskAck, ack)
	m.mu.Lock()
	rank, next, pending := src.Rank, src.NextAsk, !src.udpAskAt.IsZero()
	m.mu.Unlock()
	if rank != 7 || pending || time.Until(next) < ReaskInterval-time.Minute {
		t.Fatalf("rank %d, pending %v, next in %v", rank, pending, time.Until(next))
	}

	// No answer within udpWait: TCP next time.
	m.mu.Lock()
	src.NextAsk = time.Now().Add(-time.Second)
	m.mu.Unlock()
	m.schedule(context.Background()) // asks by UDP
	m.mu.Lock()
	src.NextAsk = time.Now().Add(-time.Second)
	src.udpAskAt = time.Now().Add(-udpWait - time.Second)
	m.mu.Unlock()
	m.schedule(context.Background()) // no answer: TCP
	m.mu.Lock()
	busy, fails := src.busy, src.udpFails
	m.mu.Unlock()
	if !busy || fails != 1 {
		t.Fatalf("after no answer: dialing %v, fails %d", busy, fails)
	}
}

// "File not found" by UDP drops the source.
func TestUDPFileNotFound(t *testing.T) {
	m, f := sxManager(t, nil)
	src := &Source{key: "198.51.100.6:4662", IP: net.ParseIP("198.51.100.6").To4(), Port: 4662, file: f,
		State: StateQueued, udpPort: 4672, udpVer: 4, udpAskAt: time.Now()}
	f.sources[src.key] = src
	m.OnClientUDP(netip.MustParseAddrPort("198.51.100.6:4672"), peer.OpFileNotFound, nil)
	if src.State != StateNoFile {
		t.Fatalf("state %v", src.State)
	}
}

// Without client UDP, a LowID of our own, or a source that did not offer
// it, nothing goes by UDP.
func TestUDPReaskableConditions(t *testing.T) {
	m, f := sxManager(t, nil)
	base := Source{IP: net.ParseIP("198.51.100.5").To4(), Port: 4662, file: f, State: StateQueued, udpPort: 4672, udpVer: 4}
	if m.udpReaskable(&base, true) {
		t.Fatal("without SendClientUDP")
	}
	m.cfg.SendClientUDP = func(netip.AddrPort, byte, []byte, ed2k.Hash) {}
	if !m.udpReaskable(&base, true) || m.udpReaskable(&base, false) {
		t.Fatal("HighID")
	}
	for name, mod := range map[string]func(*Source){
		"no port":      func(s *Source) { s.udpPort = 0 },
		"not queued":   func(s *Source) { s.State = StateNew },
		"LowID":        func(s *Source) { s.LowID, s.IP = 5, nil },
		"failed often": func(s *Source) { s.udpFails = udpMaxFails },
	} {
		s := base
		mod(&s)
		if m.udpReaskable(&s, true) {
			t.Errorf("%s: asked by UDP", name)
		}
	}
}
