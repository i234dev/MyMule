package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// A panic while handling a server packet ends that connection (MyMule
// reconnects) and is logged; the program goes on.
func TestServerPacketPanicIsContained(t *testing.T) {
	testHandleHook = func(p *proto.Packet) {
		if p.Opcode == proto.OpSearchResult {
			panic("bad packet")
		}
	}
	defer func() { testHandleHook = nil }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go fakeServer(t, ln)
	var mu sync.Mutex
	var logged []string
	logf := func(f string, a ...any) { mu.Lock(); logged = append(logged, fmt.Sprintf(f, a...)); mu.Unlock() }

	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Info{IP: addr.IP, Port: uint16(addr.Port)}, Config{Nick: "test", Log: logf})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.Search(ctx, Query{Keywords: "x"}); err == nil {
		t.Fatal("search succeeded although its answer panicked")
	}
	select {
	case <-c.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("connection not closed after the panic")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(logged, "\n"), "recovered from a panic") {
		t.Fatalf("panic not logged: %q", logged)
	}
}

// A datagram whose handling panics is skipped; the next one is handled.
func TestUDPPanicIsContained(t *testing.T) {
	u, err := ListenUDP("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	got := make(chan bool, 1)
	u.OnSources = func(*net.UDPAddr, ed2k.Hash, []Source) { got <- true }
	first := true
	testDispatchHook = func() {
		if first {
			first = false
			panic("bad datagram")
		}
	}
	defer func() { testDispatchHook = nil }()
	go u.Run()

	snd, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer snd.Close()
	u.noteAsked(net.IPv4(127, 0, 0, 1), time.Now())
	var w proto.Writer
	w.U8(proto.ProtoEDonkey)
	w.U8(OpGlobFoundSources)
	w.Bytes(make([]byte, 16))
	w.U8(1)
	w.U32(IPToU32(net.IPv4(11, 0, 0, 1)))
	w.U16(4662)
	to := u.conn.LocalAddr().(*net.UDPAddr)
	snd.WriteToUDP(w.B, to) // panics
	time.Sleep(100 * time.Millisecond)
	snd.WriteToUDP(w.B, to) // handled
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("UDP stopped after a panic")
	}
}
