package download

import (
	"context"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/server"
)

// A server cannot fill a file's source table with LowID sources (which
// cost it nothing to make up): they are capped, and room is always left
// for sources we can connect to.
func TestLowIDSourcesCapped(t *testing.T) {
	m := NewManager(Config{})
	f := &File{sources: map[string]*Source{}}
	var low []server.Source
	for i := 1; i <= 2*MaxSourcesPerFile; i++ {
		low = append(low, server.Source{ID: uint32(i), Port: 4662})
	}
	m.addSources(f, low, "198.51.100.1:4661", 0, true)
	m.addSources(f, low, "198.51.100.1:4661", 0, true)
	n := 0
	for _, s := range f.sources {
		if s.LowID != 0 {
			n++
		}
	}
	if n > MaxLowIDPerFile {
		t.Fatalf("%d LowID sources, cap %d", n, MaxLowIDPerFile)
	}
	var high []server.Source
	for i := 0; i < MaxSourcesPerFile-MaxLowIDPerFile; i++ {
		high = append(high, server.Source{ID: server.IPToU32(pub(i + 1)), Port: 4662})
	}
	if added := m.addSources(f, high, "198.51.100.1:4661", 0, true); added != len(high) {
		t.Fatalf("only %d of %d HighID sources fit next to the LowID ones", added, len(high))
	}
}

// A LowID source whose callback never comes is counted as failed each
// time, and forgotten after maxSourceFails.
func TestLowIDWithoutCallbackForgotten(t *testing.T) {
	m := NewManager(Config{MaxConns: 10})
	f := &File{sources: map[string]*Source{}, pf: &PartFile{}}
	m.files = map[ed2k.Hash]*File{{1}: f}
	src := &Source{key: "lowid:5@x", LowID: 5, server: "x", file: f}
	f.sources[src.key] = src
	for i := 0; i < maxSourceFails; i++ {
		src.callbackAt = time.Now().Add(-callbackWait - time.Second)
		src.NextAsk = time.Time{}
		m.schedule(context.Background())
	}
	if _, ok := f.sources[src.key]; ok {
		t.Fatalf("LowID source kept after %d callbacks that never came (fails=%d)", maxSourceFails, src.Fails)
	}
}
