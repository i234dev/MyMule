package download

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/server"
)

func kadFile(m *Manager, b byte) *File {
	f := &File{pf: &PartFile{Hash: ed2k.Hash{b}, Size: 1000, Name: "file"}, sources: map[string]*Source{}}
	m.files[f.pf.Hash] = f
	return f
}

// Each file is searched once an hour, at most 5 at a time, one started per
// tick; sources found are added like server ones.
func TestKadSourceSearches(t *testing.T) {
	var mu sync.Mutex
	var started []ed2k.Hash
	var finds []func(net.IP, uint16)
	fail := false
	m := NewManager(Config{Debug: func(string, ...any) {}, KadSources: func(h ed2k.Hash, size uint64, found func(net.IP, uint16)) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("not connected")
		}
		started = append(started, h)
		finds = append(finds, found)
		return nil
	}})
	for i := byte(1); i <= 7; i++ {
		kadFile(m, i)
	}
	done := kadFile(m, 8)
	done.finished = true
	now := time.Now()
	for i := range 10 {
		m.kadTick(now.Add(time.Duration(i) * time.Second))
	}
	if len(started) != kadMaxSearches {
		t.Fatalf("%d searches at once", len(started))
	}
	// After the first ones end, the rest; never the finished file.
	for i := range 10 {
		m.kadTick(now.Add(kadSearchDuration + time.Duration(i+1)*time.Second))
	}
	if len(started) != 7 {
		t.Fatalf("%d searches", len(started))
	}
	for _, h := range started {
		if h == done.pf.Hash {
			t.Fatal("finished file searched")
		}
	}
	// Not again within the hour.
	m.kadTick(now.Add(30 * time.Minute))
	if len(started) != 7 {
		t.Fatal("searched again within the hour")
	}
	m.kadTick(now.Add(KadReaskInterval + time.Minute))
	if len(started) != 8 {
		t.Fatal("not searched again after an hour")
	}
	// Sources found.
	finds[0](net.IPv4(11, 0, 0, 1), 4662)
	finds[0](net.IPv4(192, 168, 0, 1), 4662) // not public: dropped
	f := m.files[started[0]]
	if len(f.sources) != 1 {
		t.Fatalf("%d sources", len(f.sources))
	}
	// A search that cannot start is retried a minute later, not at once.
	t2 := now.Add(5 * time.Hour)
	for _, f := range m.files {
		f.kadAsked = t2.Add(-kadSearchDuration) // not due, not running
	}
	g := kadFile(m, 20)
	fail = true
	m.kadTick(t2)
	fail = false
	before := len(started)
	m.kadTick(t2.Add(time.Second))
	for _, h := range started[before:] {
		if h == g.pf.Hash {
			t.Fatal("retried at once")
		}
	}
	m.kadTick(t2.Add(2 * time.Minute))
	found := false
	for _, h := range started[before:] {
		found = found || h == g.pf.Hash
	}
	if !found {
		t.Fatal("not retried after a minute")
	}
}

// A global search also asks Kad, and works with Kad alone.
func TestGlobalSearchWithKad(t *testing.T) {
	m := NewManager(Config{KadSearch: func(ctx context.Context, q server.Query, found func(server.Result)) error {
		found(server.Result{Hash: ed2k.Hash{1}, Name: "Quiet Orchard.mkv", Size: 1000, Sources: 3})
		found(server.Result{Hash: ed2k.Hash{2}, Name: "Bright Comets.avi", Size: 0})                // no size: dropped
		found(server.Result{Hash: ed2k.Hash{3}, Name: "Blue Harbor.iso", Size: 1 << 50})            // absurd size: dropped
		found(server.Result{Hash: ed2k.Hash{1}, Name: "Quiet Orchard.mkv", Size: 1000, Sources: 3}) // again: counted once
		<-ctx.Done()
		return nil
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, nServers, err := m.SearchGlobal(ctx, server.Query{Keywords: "quiet orchard"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if nServers != 0 {
		t.Fatalf("Kad counted as %d servers", nServers)
	}
	if len(res) != 1 || res[0].Sources != 3 {
		t.Fatalf("results %+v", res)
	}
	if d := time.Since(start); d > kadSearchWait+5*time.Second {
		t.Fatalf("waited %v", d)
	}
	// Without servers and Kad: no search.
	if _, _, err := NewManager(Config{}).SearchGlobal(ctx, server.Query{Keywords: "x"}, time.Second); err != ErrNoServer {
		t.Fatalf("err %v", err)
	}
}
