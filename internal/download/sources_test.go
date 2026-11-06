package download

import (
	"net"
	"testing"

	"mymule/internal/server"
)

func srcAt(ip net.IP, port uint16) server.Source {
	return server.Source{ID: server.IPToU32(ip), Port: port}
}

func newTestFile() *File { return &File{sources: map[string]*Source{}} }

// Sources pointing at our own machine or network, or at port 0, are
// dropped: a server or a forged UDP reply could otherwise make us
// connect to anything there.
func TestAddSourcesDropsNonPublicAddresses(t *testing.T) {
	m := NewManager(Config{})
	f := newTestFile()
	srcs := []server.Source{
		srcAt(net.IPv4(127, 0, 0, 1), 4662),
		srcAt(net.IPv4(192, 168, 1, 1), 80),
		srcAt(net.IPv4(10, 0, 0, 2), 4662),
		srcAt(net.IPv4(224, 0, 0, 1), 4662),
		srcAt(net.IPv4(11, 0, 0, 1), 0),
		srcAt(net.IPv4(11, 0, 0, 1), 4662), // the only good one
	}
	if added := m.addSources(f, srcs, "srv", 0, false); added != 1 {
		t.Fatalf("added %d sources, want 1: %v", added, f.sources)
	}
	if _, ok := f.sources["11.0.0.1:4662"]; !ok {
		t.Fatalf("public source missing: %v", f.sources)
	}
}

// One reply listing every port of a host only adds a few of them.
func TestAddSourcesCapsPerIP(t *testing.T) {
	m := NewManager(Config{})
	f := newTestFile()
	var srcs []server.Source
	for p := 1; p <= 1000; p++ {
		srcs = append(srcs, srcAt(net.IPv4(11, 0, 0, 9), uint16(p)))
	}
	if added := m.addSources(f, srcs, "srv", 0, false); added != MaxSourcesPerIP {
		t.Fatalf("added %d sources from one IP, want %d", added, MaxSourcesPerIP)
	}
	// Later replies do not get around it either.
	if added := m.addSources(f, srcs[:10], "srv", 0, false); added != 0 {
		t.Fatalf("second reply added %d more from the same IP", added)
	}
}

// The list per file is bounded; dead sources make room, live ones stay.
func TestAddSourcesCapsPerFile(t *testing.T) {
	m := NewManager(Config{})
	f := newTestFile()
	batch := func(from, n int) []server.Source {
		var out []server.Source
		for i := from; i < from+n; i++ {
			out = append(out, srcAt(net.IPv4(11, 1, byte(i>>8), byte(i)), 4662))
		}
		return out
	}
	if added := m.addSources(f, batch(0, MaxSourcesPerFile+100), "srv", 0, false); added != MaxSourcesPerFile {
		t.Fatalf("added %d, want the cap %d", added, MaxSourcesPerFile)
	}
	if added := m.addSources(f, batch(5000, 10), "srv", 0, false); added != 0 {
		t.Fatalf("added %d over the cap with no dead sources", added)
	}
	dead := 0
	for _, s := range f.sources {
		if dead == 3 {
			break
		}
		s.State = StateNoFile
		dead++
	}
	if added := m.addSources(f, batch(6000, 10), "srv", 0, false); added != 3 {
		t.Fatalf("added %d after 3 sources died, want 3", added)
	}
	if len(f.sources) != MaxSourcesPerFile {
		t.Fatalf("%d sources kept, want %d", len(f.sources), MaxSourcesPerFile)
	}
}
