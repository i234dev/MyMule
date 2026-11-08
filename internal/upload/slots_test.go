package upload

import (
	"io"
	"net"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// A peer with a slot that asks only for ranges that do not exist sends
// nothing and gets nothing, so its slot lapses as if it were idle.
func TestUselessRequestsDoNotKeepSlot(t *testing.T) {
	m, h := newTestManager(t)
	ask(t, m, h, ed2k.Hash{1}, "198.51.100.1") // gets the only slot
	m.mu.Lock()
	var cl *client
	for _, c := range m.clients {
		cl = c
	}
	conn := cl.conn
	cl.lastReq = time.Now().Add(-time.Hour)
	before := cl.lastReq
	m.mu.Unlock()

	var w proto.Writer
	w.Bytes(h[:])
	for i := 0; i < 6; i++ {
		w.U32(0) // all zero: three empty ranges
	}
	m.onRequestParts(conn, &proto.Packet{Payload: w.B}, false)
	m.mu.Lock()
	kept := cl.lastReq != before
	m.mu.Unlock()
	if kept {
		t.Fatal("empty requests kept the slot alive")
	}
}

// A queued HighID peer that does not answer when its turn comes goes to
// the end of the queue instead of blocking the head, and is dropped after
// maxDialMiss tries.
func TestUnreachablePeerDoesNotBlockQueue(t *testing.T) {
	m, _ := newTestManager(t)
	dialed := make(chan string, 100)
	m.cfg.Dial = func(ip net.IP, _ uint16, _ ed2k.Hash, _ byte) { dialed <- ip.String() }
	m.cfg.Slots = 1
	m.mu.Lock()
	for i, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		cl := &client{userHash: ed2k.Hash{byte(i + 1)}, ip: net.ParseIP(ip), port: 4662, lastAsk: time.Now()}
		m.clients[idOf(cl.userHash, cl.ip)] = cl
		m.enqueueLocked(cl)
	}
	m.mu.Unlock()

	var order []string
	for round := 0; round < 2*maxDialMiss; round++ {
		m.tick()
		time.Sleep(10 * time.Millisecond)
		select {
		case ip := <-dialed:
			order = append(order, ip)
		case <-time.After(time.Second):
			t.Fatalf("round %d: nobody dialed (order %v)", round, order)
		}
		m.mu.Lock()
		for _, cl := range m.clients {
			if !cl.awaiting.IsZero() {
				cl.awaiting = time.Now().Add(-time.Second) // it never comes
			}
		}
		m.mu.Unlock()
	}
	if order[0] == order[1] {
		t.Fatalf("the same peer was dialed twice in a row: %v", order)
	}
	m.tick()
	m.tick()
	m.mu.Lock()
	n := len(m.clients)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d peers that never answered still queued", n)
	}
}

// With an upload limit below one packet (1-9 KB/s), sending still works.
func TestLimiterBelowPacketSize(t *testing.T) {
	l := newLimiter(8192)
	done := make(chan struct{})
	go func() {
		l.Wait(chunkSize)
		l.Wait(chunkSize)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("limiter of 8 KB/s never let a 10 KB packet through")
	}
}

// When a peer reaches us on a new connection, closing its older one does
// not end the upload slot it now has on the new one.
func TestOldConnectionDoesNotEndSlot(t *testing.T) {
	m, h := newTestManager(t)
	ask(t, m, h, ed2k.Hash{1}, "198.51.100.1") // slot, on its first connection
	m.mu.Lock()
	var cl *client
	for _, c := range m.clients {
		cl = c
	}
	old := cl.conn
	m.mu.Unlock()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	cur := peer.NewConn(a)
	m.Connected(cur, peer.NewHello(ed2k.Hash{1}, "peer", 0x02000000, 4662, nil, 0), net.ParseIP("198.51.100.1"))
	if !m.Serving(cur) {
		t.Fatal("setup: the slot did not move to the new connection")
	}
	m.Disconnected(old)
	if !m.Serving(cur) {
		t.Fatal("closing the old connection ended the slot on the new one")
	}
}

// We upload to a peer on connection A; our download side then reaches
// the same peer (it is also one of our sources) on connection B. The
// upload on A goes on, also after B closes.
func TestSecondConnectionKeepsUpload(t *testing.T) {
	m, h := newTestManager(t)
	ask(t, m, h, ed2k.Hash{1}, "198.51.100.1") // slot on conn A
	m.mu.Lock()
	var cl *client
	for _, c := range m.clients {
		cl = c
	}
	a := cl.conn
	m.mu.Unlock()
	if !m.Serving(a) {
		t.Fatal("setup: not serving A")
	}
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	go io.Copy(io.Discard, y)
	b := peer.NewConn(x)
	m.Connected(b, peer.NewHello(ed2k.Hash{1}, "peer", 0x02000000, 4662, nil, 0), net.ParseIP("198.51.100.1"))
	if !m.Serving(a) {
		t.Error("Serving(A) false after a second connection: A's serveUploads loop drops to 60s/unservedLimit and its requests are ignored")
	}
	before := cl.totalSent
	var w proto.Writer
	w.Bytes(h[:])
	w.U32(0)
	w.U32(0)
	w.U32(0)
	w.U32(1000)
	w.U32(0)
	w.U32(0)
	m.onRequestParts(a, &proto.Packet{Payload: w.B}, false)
	if cl.totalSent == before {
		t.Error("request on A not served")
	}
	m.Disconnected(b) // our download session ends
	m.mu.Lock()
	up := cl.uploading
	m.mu.Unlock()
	if !up {
		t.Error("slot ended when the download connection B closed, though the upload runs on A")
	}
}

// A peer with credits (ratio above 1) passes peers that have waited
// longer, by as much as its ratio; with equal ratios the order of
// arrival holds.
func TestCreditsOrderQueue(t *testing.T) {
	m, _ := newTestManager(t)
	m.cfg.Slots = 0
	ratio := map[byte]float64{3: 10}
	m.cfg.Ratio = func(h ed2k.Hash, _ net.IP) float64 {
		if r, ok := ratio[h[0]]; ok {
			return r
		}
		return 1
	}
	m.mu.Lock()
	var cls []*client
	for i, wait := range []time.Duration{50 * time.Minute, 40 * time.Minute, 6 * time.Minute} {
		cl := &client{userHash: ed2k.Hash{byte(i + 1)}, ip: net.IPv4(198, 51, 100, byte(i+1)), lastAsk: time.Now()}
		m.clients[idOf(cl.userHash, cl.ip)] = cl
		m.enqueueLocked(cl)
		cl.queuedAt = time.Now().Add(-wait)
		cls = append(cls, cl)
	}
	// Scores: 50, 40 and 6*10 = 60 minutes.
	if m.firstWaitingLocked() != cls[2] || m.rankLocked(cls[2]) != 1 || m.rankLocked(cls[0]) != 2 || m.rankLocked(cls[1]) != 3 {
		t.Fatalf("order: first %v, ranks %d %d %d", m.firstWaitingLocked().userHash[0], m.rankLocked(cls[0]), m.rankLocked(cls[1]), m.rankLocked(cls[2]))
	}
	ratio[3] = 1
	if m.firstWaitingLocked() != cls[0] || m.rankLocked(cls[2]) != 3 {
		t.Fatal("without credits the order of arrival does not hold")
	}
	m.mu.Unlock()
}
