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

func TestProgressed(t *testing.T) {
	now := time.Now()
	if got := progressed(time.Time{}, now, time.Minute, 1); !got.Equal(now) {
		t.Fatalf("unset clock: %v", now.Sub(got))
	}
	// Enough data brings the clock back to now, never past it.
	if got := progressed(now.Add(-time.Hour), now, time.Minute, 1<<20); !got.Equal(now) {
		t.Fatalf("plenty of data: %v", now.Sub(got))
	}
	// A byte buys almost nothing: the clock stays about a window behind.
	if got := progressed(now.Add(-time.Hour), now, time.Minute, 1); now.Sub(got) < time.Minute-time.Second {
		t.Fatalf("one byte moved the clock to %v ago", now.Sub(got))
	}
	// Sending at minPeerRate keeps up; sending slower falls behind.
	at := func(rate int) time.Duration {
		c, t0 := now, now
		for i := 1; i <= 600; i++ {
			c = progressed(c, t0.Add(time.Duration(i)*time.Second), time.Minute, rate)
		}
		return t0.Add(600 * time.Second).Sub(c)
	}
	if lag := at(minPeerRate); lag > time.Second {
		t.Fatalf("a peer at minPeerRate fell %v behind", lag)
	}
	if lag := at(minPeerRate / 4); lag < time.Minute-2*time.Second {
		t.Fatalf("a peer at a quarter of minPeerRate only fell %v behind", lag)
	}
}

// ageReservations makes every reservation of owner, and owner itself,
// look idle for longer than reserveStale.
func ageReservations(pf *PartFile, owner string) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	old := time.Now().Add(-reserveStale - time.Second)
	for start, o := range pf.reserved {
		if o == owner {
			pf.resAt[start] = old
		}
	}
	pf.ownerAt[owner] = old
}

// A peer that sends a byte now and then cannot keep the blocks it holds
// (the last blocks of a download, say) from a peer that sends them; a
// peer that sends real data keeps them. An empty write proves nothing.
func TestTrickleLosesReservation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int // what the holder sends into each block it holds
		keeps bool
	}{
		{"trickle", 1, false},
		{"nothing", 0, false},
		{"real data", 10240, true},
	} {
		pf, data, _ := blameSetup(t)
		held := pf.Reserve("holder:1", nil, 3)
		pf.Reserve("busy:1", nil, 1000) // everything else
		ageReservations(pf, "holder:1")
		for _, b := range held {
			end := min(b.Start+int64(tc.bytes), b.End)
			if _, err := pf.Write("holder:1", b.Start, data[b.Start:end]); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		// (A byte is worth 2 ms at minPeerRate: let it run out.)
		time.Sleep(20 * time.Millisecond)
		got := pf.Reserve("busy:1", nil, 1000)
		if kept := len(got) == 0; kept != tc.keeps {
			t.Errorf("%s: holder kept its blocks = %v (%d taken over)", tc.name, kept, len(got))
		}
		pf.Close()
	}
}

// sessionAgainst runs a download session for a one-block file against a
// fake peer, and returns how it ended (or fails after 5 s).
func sessionAgainst(t *testing.T, fake func(pc *peer.Conn, h ed2k.Hash, data []byte, stop chan struct{})) error {
	t.Helper()
	done := startSession(t, 4000, fake)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("session kept open")
		return nil
	}
}

// startSession starts a download session for a file of size bytes
// against a fake peer; the session's result comes on the channel.
func startSession(t *testing.T, size int, fake func(pc *peer.Conn, h ed2k.Hash, data []byte, stop chan struct{})) <-chan error {
	t.Helper()
	data := testData(size)
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
	ended := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		a.Close()
		b.Close()
		<-ended // the session reads the timeouts until it ends
		pf.Close()
	})
	pc := peer.NewConn(b)
	go func() { // read (and drop) what the session sends
		for {
			if _, err := pc.Read(5 * time.Second); err != nil {
				return
			}
		}
	}()
	go fake(pc, link.Hash, data, stop)
	s := &session{m: m, c: peer.NewConn(a), src: src, f: f, pf: pf, owner: src.key}
	done := make(chan error, 1)
	go func() {
		defer close(ended)
		done <- s.run(context.Background())
	}()
	return done
}

// setTimeouts sets the session time limits for one test.
func setTimeouts(t *testing.T, transfer, queued time.Duration) {
	oldT, oldQ := transferTimeout, queuedTimeout
	transferTimeout, queuedTimeout = transfer, queued
	t.Cleanup(func() { transferTimeout, queuedTimeout = oldT, oldQ })
}

func every(stop chan struct{}, send func(i int) error) {
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case <-time.After(20 * time.Millisecond):
			if send(i) != nil {
				return
			}
		}
	}
}

// A peer that gives us an upload slot and then sends one byte at a time,
// each well within transferTimeout, does not keep our connection: data
// moves the deadline only by what it is worth at minPeerRate.
func TestSessionTrickleTimesOut(t *testing.T) {
	setTimeouts(t, 300*time.Millisecond, time.Hour)
	err := sessionAgainst(t, func(pc *peer.Conn, h ed2k.Hash, data []byte, stop chan struct{}) {
		pc.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
		every(stop, func(i int) error {
			var w proto.Writer
			w.Bytes(h[:])
			w.U32(uint32(i))
			w.U32(uint32(i + 1))
			w.Bytes(data[i : i+1])
			return pc.Send(proto.ProtoEDonkey, peer.OpSendingPart, w.B)
		})
	})
	if !errors.Is(err, errStateTimeout) {
		t.Fatalf("session ended with %v, want errStateTimeout", err)
	}
}

// Empty data packets are refused outright.
func TestSessionEmptyDataRefused(t *testing.T) {
	setTimeouts(t, time.Hour, queuedTimeout)
	err := sessionAgainst(t, func(pc *peer.Conn, h ed2k.Hash, data []byte, stop chan struct{}) {
		pc.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
		every(stop, func(i int) error {
			var w proto.Writer
			w.Bytes(h[:])
			w.U32(0)
			w.U32(0)
			return pc.Send(proto.ProtoEDonkey, peer.OpSendingPart, w.B)
		})
	})
	if err == nil || errors.Is(err, errStateTimeout) {
		t.Fatalf("session ended with %v, want a refused packet", err)
	}
}

// Saying "upload slot" and "queue rank" in turn does not restart the
// deadline each time.
func TestSessionAcceptQueueAlternationTimesOut(t *testing.T) {
	setTimeouts(t, 300*time.Millisecond, time.Hour)
	err := sessionAgainst(t, func(pc *peer.Conn, h ed2k.Hash, data []byte, stop chan struct{}) {
		pc.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
		every(stop, func(i int) error {
			if i%2 == 0 {
				return pc.Send(proto.ProtoEDonkey, peer.OpQueueRank, []byte{5, 0, 0, 0})
			}
			return pc.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
		})
	})
	if !errors.Is(err, errStateTimeout) {
		t.Fatalf("session ended with %v, want errStateTimeout", err)
	}
}

// sendPart sends data[start:end] as one data packet.
func sendPart(pc *peer.Conn, h ed2k.Hash, data []byte, start, end int) error {
	var w proto.Writer
	w.Bytes(h[:])
	w.U32(uint32(start))
	w.U32(uint32(end))
	w.Bytes(data[start:end])
	return pc.Send(proto.ProtoEDonkey, peer.OpSendingPart, w.B)
}

// Bytes already on disk are no progress: a peer sending the same bytes
// again and again keeps neither its blocks nor the connection.
func TestResentBytesAreNoProgress(t *testing.T) {
	pf, data, _ := blameSetup(t)
	held := pf.Reserve("holder:1", nil, 3)
	pf.Reserve("busy:1", nil, 1000)
	b := held[0]
	if _, err := pf.Write("holder:1", b.Start, data[b.Start:b.Start+1024]); err != nil {
		t.Fatal(err)
	}
	ageReservations(pf, "holder:1")
	for range 50 {
		if _, err := pf.Write("holder:1", b.Start, data[b.Start:b.Start+1024]); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := pf.Reserve("busy:1", nil, 1000); len(got) != len(held) {
		t.Fatalf("took over %d of %d blocks of a peer re-sending old bytes", len(got), len(held))
	}
}

func TestSessionResentBytesTimeOut(t *testing.T) {
	setTimeouts(t, 300*time.Millisecond, time.Hour)
	err := sessionAgainst(t, func(pc *peer.Conn, h ed2k.Hash, data []byte, stop chan struct{}) {
		pc.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
		every(stop, func(i int) error { return sendPart(pc, h, data, 0, 1000) })
	})
	if !errors.Is(err, errStateTimeout) {
		t.Fatalf("session ended with %v, want errStateTimeout", err)
	}
}

// A peer that sends at a fair rate keeps the connection well past
// transferTimeout, and one that queues us first and then gives us a slot
// gets the transfer deadline from the slot.
func TestSessionHonestPeerKeepsConnection(t *testing.T) {
	setTimeouts(t, 300*time.Millisecond, 50*time.Millisecond)
	done := startSession(t, 400000, func(pc *peer.Conn, h ed2k.Hash, data []byte, stop chan struct{}) {
		pc.Send(proto.ProtoEDonkey, peer.OpQueueRank, []byte{5, 0, 0, 0})
		pc.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
		time.Sleep(100 * time.Millisecond)                                                  // longer than the queued wait
		every(stop, func(i int) error { return sendPart(pc, h, data, i*1000, (i+1)*1000) }) // 50 KB/s
	})
	select {
	case err := <-done:
		t.Fatalf("an honest peer's session ended: %v", err)
	case <-time.After(time.Second):
	}
}

// A peer that comes back long after it last sent anything starts a fresh
// clock with the blocks it gets, like a new peer.
func TestReconnectingOwnerStartsFresh(t *testing.T) {
	pf, _, _ := blameSetup(t)
	ageReservations(pf, "back:1")
	if len(pf.Reserve("back:1", nil, 3)) == 0 {
		t.Fatal("setup: no blocks")
	}
	pf.mu.Lock()
	lag := time.Since(pf.ownerAt["back:1"])
	pf.mu.Unlock()
	if lag > time.Second {
		t.Fatalf("returning peer's clock is %v behind", lag)
	}
}

// When an isolated part is started over, its next single peer starts a
// fresh clock too.
func TestAbandonedIsolatedPartClockReset(t *testing.T) {
	pf, data, _ := blameSetup(t)
	first := []bool{true, false, false}
	pf.mu.Lock()
	pf.isolate[0] = true
	pf.mu.Unlock()
	b := pf.Reserve("gone:1", first, 1)
	pf.Write("gone:1", b[0].Start, data[b[0].Start:b[0].End])
	pf.Release("gone:1", b[0])
	pf.mu.Lock()
	pf.lastWr[0] = pf.lastWr[0].Add(-isolateAbandon - 1)
	pf.mu.Unlock()
	if len(pf.Reserve("next:1", first, 1)) == 0 {
		t.Fatal("abandoned part not taken over")
	}
	pf.mu.Lock()
	_, kept := pf.lastWr[0]
	pf.mu.Unlock()
	if kept {
		t.Fatal("the abandoned part kept its old clock")
	}
}
