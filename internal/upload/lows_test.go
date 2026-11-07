package upload

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/share"
)

// countingFile is a shared file that counts reads (each read of a real
// file opens and checks it).
type countingFile struct {
	h     ed2k.Hash
	reads atomic.Int32
}

func (f *countingFile) FileHash() ed2k.Hash     { return f.h }
func (f *countingFile) FileName() string        { return "counted.bin" }
func (f *countingFile) FileSize() int64         { return 400000 }
func (f *countingFile) PartHashes() []ed2k.Hash { return nil }
func (f *countingFile) Avail() []bool           { return nil }
func (f *countingFile) Partial() bool           { return false }
func (f *countingFile) ReadAt(p []byte, off int64) (int, error) {
	f.reads.Add(1)
	return len(p), nil
}

func onlyClient(m *Manager) *client {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		return c
	}
	return nil
}

func requestRange(m *Manager, c *peer.Conn, h ed2k.Hash, start, end uint32) {
	var w proto.Writer
	w.Bytes(h[:])
	w.U32(start)
	w.U32(0)
	w.U32(0)
	w.U32(end)
	w.U32(0)
	w.U32(0)
	m.onRequestParts(c, &proto.Packet{Payload: w.B}, false)
}

// A request is read from the disk once, not once per 10 KB packet.
func TestRequestReadOnce(t *testing.T) {
	m, _ := newTestManager(t)
	f := &countingFile{h: ed2k.Hash{0xc0}}
	m.cfg.Library.SetPartial(f.h, f)
	ask(t, m, f.h, ed2k.Hash{1}, "198.51.100.1")
	cl := onlyClient(m)
	requestRange(m, cl.conn, f.h, 0, 300000)
	if n := f.reads.Load(); n != 1 {
		t.Fatalf("%d reads for one request", n)
	}
	if cl.sent != 300000 {
		t.Fatalf("sent %d", cl.sent)
	}
}

// A turn is not ended for peers that could not take the slot anyway (an
// unconnected LowID peer): that only gave it back at once, again and again.
func TestNoRotationForPeersThatCannotTakeTheSlot(t *testing.T) {
	m, h := newTestManager(t)
	ask(t, m, h, ed2k.Hash{1}, "198.51.100.1")
	cl := onlyClient(m)
	m.mu.Lock()
	lowID := &client{userHash: ed2k.Hash{2}, ip: net.ParseIP("198.51.100.2"), lowID: true, lastAsk: time.Now()}
	m.clients[idOf(lowID.userHash, lowID.ip)] = lowID
	m.enqueueLocked(lowID)
	cl.sent = slotQuota
	m.mu.Unlock()
	requestRange(m, cl.conn, h, 0, 1000)
	m.mu.Lock()
	kept := cl.uploading
	m.mu.Unlock()
	if !kept {
		t.Fatal("turn ended for a peer that cannot take the slot")
	}

	// A peer we can dial does get it.
	m.cfg.Dial = func(net.IP, uint16) {}
	m.mu.Lock()
	lowID.lowID = false
	m.mu.Unlock()
	requestRange(m, cl.conn, h, 0, 1000)
	m.mu.Lock()
	kept = cl.uploading
	m.mu.Unlock()
	if kept {
		t.Fatal("turn not ended for a waiting peer")
	}
}

// A peer that does not read its connection does not stall the scheduler.
func TestTickDoesNotWaitForSends(t *testing.T) {
	m, _ := newTestManager(t)
	a, b := net.Pipe() // nobody reads b
	t.Cleanup(func() { a.Close(); b.Close() })
	m.mu.Lock()
	cl := &client{userHash: ed2k.Hash{1}, ip: net.ParseIP("198.51.100.1"), conn: peer.NewConn(a),
		uploading: true, lastReq: time.Now().Add(-time.Hour), lastAsk: time.Now()}
	m.clients[idOf(cl.userHash, cl.ip)] = cl
	m.byConn[cl.conn] = cl
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.tick(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick waited for a peer that does not read")
	}
}

// A peer that asks by itself has shown it is there: dials it missed long
// ago no longer count towards dropping it.
func TestAskingClearsMissedDials(t *testing.T) {
	m, h := newTestManager(t)
	ask(t, m, h, ed2k.Hash{1}, "198.51.100.1")
	ask(t, m, h, ed2k.Hash{2}, "198.51.100.2")
	m.mu.Lock()
	var queued *client
	for _, c := range m.clients {
		if !c.uploading {
			queued = c
		}
	}
	queued.dialMiss = maxDialMiss - 1
	m.mu.Unlock()
	ask(t, m, h, ed2k.Hash{2}, "198.51.100.2")
	m.mu.Lock()
	n := queued.dialMiss
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("dialMiss %d after asking", n)
	}
}

var _ share.File = (*countingFile)(nil)

// A turn that ends in a tick is not given again in that same tick: its
// "out of parts" and the new accept would go out from separate goroutines
// and could arrive swapped.
func TestEndedTurnNotRegrantedInSameTick(t *testing.T) {
	m, _ := newTestManager(t)
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	m.mu.Lock()
	cl := &client{userHash: ed2k.Hash{1}, ip: net.ParseIP("198.51.100.1"), conn: peer.NewConn(a),
		uploading: true, lastReq: time.Now().Add(-time.Hour), lastAsk: time.Now()}
	m.clients[idOf(cl.userHash, cl.ip)] = cl
	m.byConn[cl.conn] = cl
	m.mu.Unlock()
	m.tick()
	m.mu.Lock()
	again := cl.uploading
	m.mu.Unlock()
	if again {
		t.Fatal("ended turn given again in the same tick")
	}
}
