package upload

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/share"
)

func TestValidRange(t *testing.T) {
	const size = 10 << 20
	for _, c := range []struct {
		s, e uint64
		ok   bool
	}{
		{0, 1000, true},
		{size - 10, size, true},
		{1000, 1000, false},
		{2000, 1000, false},
		{size - 10, size + 1, false},
		{0, maxRange + 1, false},
		{1 << 63, 1<<63 + 100, false}, // negative as int64
		{^uint64(0) - 100, 100, false},
	} {
		if got := validRange(c.s, c.e, size); got != c.ok {
			t.Errorf("validRange(%d, %d) = %v, want %v", c.s, c.e, got, c.ok)
		}
	}
}

// newTestManager shares one file and returns the manager and its hash.
func newTestManager(t *testing.T) (*Manager, ed2k.Hash) {
	t.Helper()
	dir := t.TempDir()
	data := bytes.Repeat([]byte("shared data "), 1000)
	p := filepath.Join(dir, "file.bin")
	os.WriteFile(p, data, 0o644)
	fh, err := ed2k.HashReader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	lib := share.NewLibrary([]string{dir}, filepath.Join(t.TempDir(), "known.json"), nil)
	lib.AddComplete(p, fh.Root, nil)
	return New(Config{Slots: 1, Library: lib}), fh.Root
}

// ask makes a peer with user hash uh from ip ask for the file.
func ask(t *testing.T, m *Manager, h ed2k.Hash, uh ed2k.Hash, ip string) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go io.Copy(io.Discard, b)
	hello := peer.NewHello(uh, "peer", 0x02000000, 4662, nil, 0)
	m.onStartUpload(peer.NewConn(a), hello, net.ParseIP(ip), &proto.Packet{Payload: h[:]})
}

// Someone sending another user's hash from another address gets a place
// of its own; it cannot take over that user's place in the queue.
func TestQueueIdentityIncludesIP(t *testing.T) {
	m, h := newTestManager(t)
	ask(t, m, h, ed2k.Hash{1}, "198.51.100.1") // gets the only slot
	ask(t, m, h, ed2k.Hash{2}, "198.51.100.2") // queued first
	ask(t, m, h, ed2k.Hash{2}, "203.0.113.9")  // same hash, other IP
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) != 2 {
		t.Fatalf("%d in the queue, want 2 separate places", len(m.queue))
	}
	if !m.queue[0].ip.Equal(net.ParseIP("198.51.100.2")) {
		t.Fatalf("first place now belongs to %s", m.queue[0].ip)
	}
}

// One address cannot fill the queue.
func TestQueueCapPerIP(t *testing.T) {
	m, h := newTestManager(t)
	for i := 0; i < 50; i++ {
		ask(t, m, h, ed2k.Hash{byte(i + 1)}, "198.51.100.7")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.clients); n != maxQueuePerIP {
		t.Fatalf("%d entries from one IP, want %d", n, maxQueuePerIP)
	}
}
