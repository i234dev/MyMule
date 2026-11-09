package upload

import (
	"net"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
)

// A peer waiting in our queue that asks by UDP, from the UDP port its
// hello gave, gets its place and keeps it; a file we do not share gets
// "not found"; a peer that is not waiting gets nothing (it asks by TCP),
// unless the queue is full.
func TestReaskPing(t *testing.T) {
	m, h := newTestManager(t)
	m.mu.Lock()
	cl := &client{userHash: ed2k.Hash{7}, ip: net.ParseIP("198.51.100.5"), port: 4662, udpPort: 4672, udpVer: 4,
		file: h, lastAsk: time.Now().Add(-50 * time.Minute), crypt: 1}
	m.clients[idOf(cl.userHash, cl.ip)] = cl
	m.enqueueLocked(cl)
	m.mu.Unlock()

	r, ok := m.ReaskPing(cl.ip, 4672, peer.EncodeReaskFilePing(4, h, nil, 0))
	if !ok || r.Op != peer.OpReaskAck || r.UserHash != cl.userHash {
		t.Fatalf("%+v %v", r, ok)
	}
	if _, rank, err := peer.DecodeReaskAck(r.Payload, 4); err != nil || rank != 1 {
		t.Fatalf("rank %d: %v", rank, err)
	}
	m.mu.Lock()
	refreshed := time.Since(cl.lastAsk) < time.Minute
	m.mu.Unlock()
	if !refreshed {
		t.Fatal("its place was not kept")
	}
	if _, ok := m.ReaskPing(cl.ip, 9999, peer.EncodeReaskFilePing(4, h, nil, 0)); ok {
		t.Fatal("answered another port of its address")
	}
	if r, ok := m.ReaskPing(cl.ip, 4672, peer.EncodeReaskFilePing(4, ed2k.Hash{0xEE}, nil, 0)); !ok || r.Op != peer.OpFileNotFound {
		t.Fatalf("unknown file: %+v", r)
	}
	if _, ok := m.ReaskPing(net.ParseIP("198.51.100.6"), 4672, peer.EncodeReaskFilePing(4, h, nil, 0)); ok {
		t.Fatal("answered a peer that is not waiting")
	}
	if _, ok := m.ReaskPing(cl.ip, 4672, []byte{1, 2, 3}); ok {
		t.Fatal("answered junk")
	}
}
