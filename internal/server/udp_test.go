package server

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// fakeUDPServer answers source requests and searches like a real server,
// concatenating two packets in one datagram as servers do.
func fakeUDPServer(t *testing.T) (*net.UDPConn, Info) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 2 || buf[0] != proto.ProtoEDonkey {
				continue
			}
			r := proto.NewReader(buf[2:n])
			var w proto.Writer
			switch buf[1] {
			case OpGlobGetSources2:
				for r.Len() >= 20 {
					h := r.Hash()
					if r.U32() == 0 {
						r.U64()
					}
					w.U8(proto.ProtoEDonkey)
					w.U8(OpGlobFoundSources)
					w.Bytes(h[:])
					w.U8(1)
					w.U32(IPToU32(net.IPv4(9, 9, 9, 9)))
					w.U16(4662)
				}
			case OpGlobSearchReq:
				for i := 0; i < 2; i++ {
					w.U8(proto.ProtoEDonkey)
					w.U8(OpGlobSearchRes)
					w.Bytes([]byte{byte(i), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
					w.U32(0)
					w.U16(0)
					w.U32(3)
					w.WriteTag(proto.StrTag(proto.FTFileName, "result.iso"))
					w.WriteTag(proto.IntTag(proto.FTFileSize, 734003200))
					w.WriteTag(proto.IntTag(proto.FTSources, 5))
				}
			}
			c.WriteToUDP(w.B, from)
		}
	}()
	a := c.LocalAddr().(*net.UDPAddr)
	// The client sends to TCP port + 4.
	return c, Info{IP: a.IP, Port: uint16(a.Port - 4)}
}

func TestUDPSourcesAndSearch(t *testing.T) {
	srvConn, info := fakeUDPServer(t)
	defer srvConn.Close()

	u, err := ListenUDP("127.0.0.1") // loopback only: no firewall prompt
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	var mu sync.Mutex
	got := map[ed2k.Hash][]Source{}
	u.OnSources = func(from *net.UDPAddr, h ed2k.Hash, srcs []Source) {
		mu.Lock()
		got[h] = srcs
		mu.Unlock()
	}
	go u.Run()

	files := []FileReq{{Hash: ed2k.Hash{1}, Size: 1000}, {Hash: ed2k.Hash{2}, Size: 5 << 30}}
	if err := u.GetSources(info, files); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(got) != 2 || got[ed2k.Hash{2}][0].Addr() != "9.9.9.9:4662" {
		t.Fatalf("sources: %v", got)
	}
	mu.Unlock()

	var results []Result
	err = u.Search(context.Background(), []Info{info}, Query{Keywords: "x"}, 500*time.Millisecond,
		func(from *net.UDPAddr, r Result) {
			if ServerKey(from) != info.Addr() {
				t.Errorf("server key %s != %s", ServerKey(from), info.Addr())
			}
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 2 || results[0].Name != "result.iso" || results[0].Sources != 5 {
		t.Fatalf("results: %+v", results)
	}
}

// Only servers we sent something to may answer over UDP: anyone else who
// knows our port could otherwise inject fake sources or search results.
func TestUDPIgnoresUnsolicitedReplies(t *testing.T) {
	u := &UDP{searches: map[int]*udpSearch{}, asked: map[string]time.Time{}}
	calls := 0
	u.OnSources = func(*net.UDPAddr, ed2k.Hash, []Source) { calls++ }

	var w proto.Writer
	w.U8(proto.ProtoEDonkey)
	w.U8(OpGlobFoundSources)
	w.Bytes(make([]byte, 16))
	w.U8(1)
	w.U32(IPToU32(net.IPv4(9, 9, 9, 9)))
	w.U16(4662)
	from := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 4665}

	u.dispatch(from, w.B)
	if calls != 0 {
		t.Fatal("reply from a server we never asked was accepted")
	}
	u.noteAsked(from.IP, time.Now().Add(-udpReplyWindow-time.Minute))
	u.dispatch(from, w.B)
	if calls != 0 {
		t.Fatal("reply accepted long after we last asked that server")
	}
	u.noteAsked(from.IP, time.Now())
	// Some servers reply from another port than the one we sent to.
	u.dispatch(&net.UDPAddr{IP: from.IP, Port: 4672}, w.B)
	if calls != 1 {
		t.Fatalf("reply from a server we just asked: %d calls, want 1", calls)
	}
}

func TestUDPSearchDropsBadSizes(t *testing.T) {
	u := &UDP{searches: map[int]*udpSearch{}, asked: map[string]time.Time{}}
	var got []Result
	u.searches[0] = &udpSearch{cb: func(_ *net.UDPAddr, r Result) { got = append(got, r) },
		sent: map[string]time.Time{"203.0.113.7": time.Now()}}
	var w proto.Writer
	for i, hi := range []uint32{0, 1 << 28} {
		w.U8(proto.ProtoEDonkey)
		w.U8(OpGlobSearchRes)
		w.Bytes([]byte{byte(i), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
		w.U32(0)
		w.U16(0)
		w.U32(2)
		w.WriteTag(proto.IntTag(proto.FTFileSize, 1000))
		w.WriteTag(proto.IntTag(proto.FTFileSizeHi, hi))
	}
	from := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 4665}
	u.noteAsked(from.IP, time.Now())
	u.dispatch(from, w.B)
	if len(got) != 1 || got[0].Size != 1000 {
		t.Fatalf("got %+v, want only the 1000-byte result", got)
	}
}

// Global search answers carry no search id. An answer goes to the search
// that asked that server most recently, and not to a search that has not
// asked it yet (that is a late answer to an earlier search).
func TestUDPSearchAnswersRouted(t *testing.T) {
	u := &UDP{searches: map[int]*udpSearch{}, asked: map[string]time.Time{}}
	var old, cur []string
	u.searches[1] = &udpSearch{cb: func(_ *net.UDPAddr, r Result) { old = append(old, r.Name) },
		sent: map[string]time.Time{"203.0.113.7": time.Now().Add(-5 * time.Second), "203.0.113.8": time.Now()}}
	u.searches[2] = &udpSearch{cb: func(_ *net.UDPAddr, r Result) { cur = append(cur, r.Name) },
		sent: map[string]time.Time{"203.0.113.7": time.Now()}}
	answer := func(ip net.IP, name string) {
		var w proto.Writer
		w.U8(proto.ProtoEDonkey)
		w.U8(OpGlobSearchRes)
		w.Bytes(make([]byte, 16))
		w.U32(0)
		w.U16(0)
		w.U32(2)
		w.WriteTag(proto.StrTag(proto.FTFileName, name))
		w.WriteTag(proto.IntTag(proto.FTFileSize, 1000))
		u.noteAsked(ip, time.Now())
		u.dispatch(&net.UDPAddr{IP: ip, Port: 4665}, w.B)
	}
	answer(net.IPv4(203, 0, 113, 7), "a") // asked by both: the newer search
	answer(net.IPv4(203, 0, 113, 8), "b") // asked only by the older one
	answer(net.IPv4(203, 0, 113, 9), "c") // asked by neither search
	if len(cur) != 1 || cur[0] != "a" || len(old) != 1 || old[0] != "b" {
		t.Fatalf("newer search got %v, older got %v", cur, old)
	}
}

// Two searches running at the same time ask the same server; its late
// answers to the first one must not end up in the second (the *arr apps
// search several titles at once).
func TestUDPOverlappingSearchesKeepTheirAnswers(t *testing.T) {
	u := &UDP{searches: map[int]*udpSearch{}, asked: map[string]time.Time{}}
	var first, second []string
	now := time.Now()
	u.searches[1] = &udpSearch{q: Query{Keywords: "Blue Harbor"},
		cb:   func(_ *net.UDPAddr, r Result) { first = append(first, r.Name) },
		sent: map[string]time.Time{"203.0.113.7": now.Add(-2 * time.Second)}}
	u.searches[2] = &udpSearch{q: Query{Keywords: "quiet orchard", MinSize: 500},
		cb:   func(_ *net.UDPAddr, r Result) { second = append(second, r.Name) },
		sent: map[string]time.Time{"203.0.113.7": now}}
	answer := func(name string, size uint32) {
		var w proto.Writer
		w.U8(proto.ProtoEDonkey)
		w.U8(OpGlobSearchRes)
		w.Bytes(make([]byte, 16))
		w.U32(0)
		w.U16(0)
		w.U32(2)
		w.WriteTag(proto.StrTag(proto.FTFileName, name))
		w.WriteTag(proto.IntTag(proto.FTFileSize, size))
		u.noteAsked(net.IPv4(203, 0, 113, 7), time.Now())
		u.dispatch(&net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 4665}, w.B)
	}
	answer("blue.harbor.s01e01.mkv", 1000) // late answer to the first search
	answer("Quiet Orchard 1080p.mkv", 1000)
	answer("Quiet Orchard sample.mkv", 100) // too small for either: the newest
	answer("unrelated name.avi", 1000)      // fits neither: the newest
	if len(first) != 1 || first[0] != "blue.harbor.s01e01.mkv" || len(second) != 3 {
		t.Fatalf("first search got %v, second got %v", first, second)
	}
}

func TestQueryFits(t *testing.T) {
	q := Query{Keywords: "Silver  Comet", MinSize: 10, MaxSize: 20}
	for _, c := range []struct {
		name string
		size uint64
		want bool
	}{
		{"the.silver.comet.mkv", 15, true},
		{"SILVER-COMET", 10, true},
		{"silver.mkv", 15, false},
		{"silver comet", 9, false},
		{"silver comet", 21, false},
	} {
		if got := q.fits(Result{Name: c.name, Size: c.size}); got != c.want {
			t.Errorf("%q %d: %v", c.name, c.size, got)
		}
	}
	if !(Query{Keywords: "x"}).fits(Result{Name: "X", Size: 1 << 40}) {
		t.Error("no size limits refused a size")
	}
}

// Punctuation and accents, which servers ignore, do not send a result to
// the wrong search.
func TestQueryFitsLoosely(t *testing.T) {
	r := Result{Name: "Bright.Comets.S01E01.mkv", Size: 1}
	if !(Query{Keywords: "bright's comets"}).fits(r) {
		t.Error("apostrophe")
	}
	if !(Query{Keywords: "brîght comets"}).fits(r) {
		t.Error("accent")
	}
	if (Query{Keywords: "dark comets"}).fits(r) {
		t.Error("other words")
	}
}
