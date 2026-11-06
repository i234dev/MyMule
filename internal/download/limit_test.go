package download

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"mymule/internal/peer"
	"mymule/internal/proto"
)

func TestConnLimiter(t *testing.T) {
	l := connLimiter{maxTotal: 7, maxPerIP: 5}
	for i := 0; i < 5; i++ {
		if !l.acquire("192.0.2.1") {
			t.Fatalf("connection %d from one IP refused", i+1)
		}
	}
	if l.acquire("192.0.2.1") {
		t.Fatal("6th connection from one IP accepted")
	}
	if !l.acquire("192.0.2.2") || !l.acquire("192.0.2.3") {
		t.Fatal("other IPs refused below the total limit")
	}
	if l.acquire("192.0.2.4") {
		t.Fatal("connection accepted over the total limit")
	}
	l.release("192.0.2.1")
	if !l.acquire("192.0.2.1") {
		t.Fatal("slot not freed by release")
	}
	l.release("198.51.100.9") // never acquired: must not corrupt the counts
	if l.acquire("192.0.2.4") {
		t.Fatal("bogus release freed a slot")
	}
}

// The accept loop closes connections over the per-IP limit at once and
// keeps the others.
func TestAcceptLoopLimitsPerIP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	m.wg.Add(1)
	go m.acceptLoop(ctx, ln)
	defer func() { cancel(); ln.Close(); m.wg.Wait() }()

	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	closed := func(c net.Conn) bool {
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		return err == io.EOF || isReset(err)
	}
	for i := 0; i < MaxIncomingPerIP; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	extra, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conns = append(conns, extra)
	if !closed(extra) {
		t.Fatalf("connection %d from one IP was kept open", MaxIncomingPerIP+1)
	}
	if closed(conns[0]) {
		t.Fatal("a connection within the limit was closed")
	}
}

func isReset(err error) bool {
	if err == nil {
		return false
	}
	ne, ok := err.(net.Error)
	return !ok || !ne.Timeout()
}

// A peer that keeps sending packets but never says hello must be dropped
// when the handshake deadline passes, not get a fresh timeout per packet.
func TestHandshakeHasOneDeadline(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	defer func() { handshakeTimeout = old }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer c.Close()
		go io.Copy(io.Discard, c)
		// An eMule info packet every 250 ms: each one arrives before a
		// per-read timeout of 300 ms would expire.
		info := (&proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpEMuleInfo}).Encode()
		for i := 0; i < 10; i++ {
			if _, err := c.Write(info); err != nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
	nc, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	m := NewManager(Config{})
	start := time.Now()
	if _, err := m.handshake(peer.NewConn(nc), false); err == nil {
		t.Fatal("handshake without hello succeeded")
	}
	if took := time.Since(start); took > 600*time.Millisecond {
		t.Fatalf("handshake gave up after %v, want about %v", took, handshakeTimeout)
	}
}
