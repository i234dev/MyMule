package download

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// chattyPeer answers our requests with an upload slot (or a queue rank),
// then sends packets that carry nothing useful, forever.
func chattyPeer(nc net.Conn, accept bool, stop chan struct{}) {
	pc := peer.NewConn(nc)
	go func() {
		for {
			if _, err := pc.Read(5 * time.Second); err != nil {
				return
			}
		}
	}()
	if accept {
		pc.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
	} else {
		pc.Send(proto.ProtoEDonkey, peer.OpQueueRank, []byte{5, 0, 0, 0})
	}
	for {
		select {
		case <-stop:
			return
		case <-time.After(30 * time.Millisecond):
			var err error
			if accept {
				err = pc.Send(proto.ProtoEMule, peer.OpEMuleInfo, peer.EMuleInfoAnswer())
			} else {
				err = pc.Send(proto.ProtoEDonkey, peer.OpQueueRank, []byte{5, 0, 0, 0})
			}
			if err != nil {
				return
			}
		}
	}
}

// A peer cannot hold one of our download connections for ever by sending
// packets that carry no data (while "uploading" to us) or the same queue
// rank again and again.
func TestSessionStateDeadlines(t *testing.T) {
	oldT, oldQ := transferTimeout, queuedTimeout
	transferTimeout, queuedTimeout = 300*time.Millisecond, 300*time.Millisecond
	defer func() { transferTimeout, queuedTimeout = oldT, oldQ }()
	for _, accept := range []bool{true, false} {
		data := testData(4000)
		link, _ := linkFor(t, "film.mkv", data)
		pf, err := create(t.TempDir(), link, false)
		if err != nil {
			t.Fatal(err)
		}
		m := NewManager(Config{})
		f := &File{pf: pf, sources: map[string]*Source{}}
		m.files = map[ed2k.Hash]*File{link.Hash: f}
		src := &Source{key: "198.51.100.9:4662", file: f}
		a, b := net.Pipe()
		stop := make(chan struct{})
		go chattyPeer(b, accept, stop)
		s := &session{m: m, c: peer.NewConn(a), src: src, f: f, pf: pf, owner: src.key}
		done := make(chan error, 1)
		go func() { done <- s.run(context.Background()) }()
		select {
		case err := <-done:
			if !errors.Is(err, errStateTimeout) {
				t.Errorf("accept=%v: session ended with %v, want errStateTimeout", accept, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("accept=%v: session kept open by a peer sending nothing useful", accept)
		}
		close(stop)
		a.Close()
		b.Close()
		pf.Close()
	}
}

// The callback limit counts connection attempts, not whole sessions: a
// peer that keeps the session open does not keep the callback slot.
func TestCallbackSlotFreedAfterConnect(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(3 * time.Second) // holds the connection, says nothing
		}
	}()
	m := NewManager(Config{})
	freed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		m.dialCallback(ctx, ln.Addr().String(), func() { close(freed) })
		close(ended)
	}()
	select {
	case <-freed:
	case <-time.After(time.Second):
		t.Fatal("callback slot still held while the session runs")
	}
	// Leave nothing running for the next tests.
	cancel()
	m.mu.Lock()
	for c := range m.conns {
		c.Close()
	}
	m.mu.Unlock()
	<-ended
}

// eMule announces size/PartSize+1 parts for files whose size is an exact
// multiple of the part size. That status is understood, not taken as
// malformed (which made every such source look complete).
func TestFileStatusFromEMule(t *testing.T) {
	m := NewManager(Config{})
	link := ed2k.Link{Name: "film.mkv", Size: ed2k.PartSize * 2, Hash: ed2k.Hash{3}}
	pf, err := create(t.TempDir(), link, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	f := &File{pf: pf, sources: map[string]*Source{}}
	src := &Source{key: "198.51.100.9:4662", file: f}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	s := &session{m: m, c: peer.NewConn(a), src: src, f: f, pf: pf, owner: src.key}
	var w proto.Writer
	w.Bytes(link.Hash[:])
	w.U16(3)               // eMule's count for two parts
	w.Bytes([]byte{0b001}) // has part 0 only
	s.handle(&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: peer.OpFileStatus, Payload: w.B})
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(src.Avail) != 2 || !src.Avail[0] || src.Avail[1] {
		t.Fatalf("availability %v, want [true false]", src.Avail)
	}
}
