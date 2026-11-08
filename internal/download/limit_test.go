package download

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
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

// A panic inside a peer session (a bug some input triggers) ends that
// session only: the manager keeps accepting connections and is not left
// locked.
func TestSessionPanicIsContained(t *testing.T) {
	var mu sync.Mutex
	panics := 0
	testSessionHook = func() {
		mu.Lock()
		panics++
		mu.Unlock()
		panic("bad session")
	}
	defer func() { testSessionHook = nil }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logged := make(chan string, 10)
	m := NewManager(Config{Log: func(f string, a ...any) { logged <- fmt.Sprintf(f, a...) }})
	ctx, cancel := context.WithCancel(context.Background())
	m.wg.Add(1)
	go m.acceptLoop(ctx, ln)
	defer func() { cancel(); ln.Close(); m.wg.Wait() }()

	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte{proto.ProtoEDonkey}) // a plain connection: the session starts
		select {
		case msg := <-logged:
			if !strings.Contains(msg, "recovered from a panic") {
				t.Fatalf("log: %q", msg)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("session %d: panic not reported", i+1)
		}
		c.Close()
	}
	// The panic handler itself briefly takes the lock to see that it was
	// released, after logging: try for a while.
	free := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if m.mu.TryLock() {
			m.mu.Unlock()
			free = true
			break
		}
	}
	if !free {
		t.Fatal("manager lock left held")
	}
	mu.Lock()
	defer mu.Unlock()
	if panics != 2 {
		t.Fatalf("%d sessions ran, want 2 (the manager kept accepting)", panics)
	}
}

// A flood of chat messages from peers is not all written to the log.
func TestPeerMessagesAreCapped(t *testing.T) {
	var mu sync.Mutex
	lines := 0
	m := NewManager(Config{Log: func(string, ...any) { mu.Lock(); lines++; mu.Unlock() }})
	for i := 0; i < 1000; i++ {
		m.logPeerMessage("198.51.100.7:4662", "spam")
	}
	if lines != maxPeerMessagesPerMinute {
		t.Fatalf("%d lines logged for 1000 messages, want %d", lines, maxPeerMessagesPerMinute)
	}
	// The next minute: a summary of what was dropped, then messages again.
	m.msgMu.Lock()
	m.msgWindow = m.msgWindow.Add(-time.Minute)
	m.msgMu.Unlock()
	m.logPeerMessage("198.51.100.7:4662", "hello")
	if lines != maxPeerMessagesPerMinute+2 {
		t.Fatalf("after a minute: %d lines, want the summary and the message", lines-maxPeerMessagesPerMinute)
	}
}
