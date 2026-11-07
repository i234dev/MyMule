package kad

import (
	"net/netip"
	"testing"
	"time"

	"mymule/internal/proto"
)

// A full publish packet from an honest node without our key waits whole,
// and is not answered as overloaded.
func TestUnprovenPacketWaitsWhole(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, _ := storeNode(t, key)
	x := netip.AddrPortFrom(pubIP(90, false), 4672)
	var w proto.Writer
	WriteID(&w, key)
	w.U16(filesPerPublish)
	for i := range filesPerPublish {
		WriteID(&w, RandomID())
		WriteTags(&w, []proto.Tag{StrTag(TagFileName, "Quiet Orchard.mkv"), IntTag(TagFileSize, uint64(1+i))})
	}
	fn.inject(x, a, rawPacket(OpPublishKeyReq, w.B))
	if n := len(a.pending[x.Addr()].items); n != filesPerPublish {
		t.Fatalf("%d of %d waiting", n, filesPerPublish)
	}
	var load byte = 255
	for _, d := range fn.sent[x] {
		if p, err := DecodePacket(d); err == nil && p.Opcode == OpPublishRes {
			r := proto.NewReader(p.Payload)
			ReadID(r)
			load = r.U8()
		}
	}
	if load >= overloadedLoad {
		t.Fatalf("answered with load %d", load)
	}
}

// A full table of waiting publishes makes room for new ones.
func TestPendingTableFull(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	for i := range maxPendingIPs {
		a.pending[pubIP(10000+i, false)] = &pendingPub{at: fn.now.Add(time.Duration(i) * time.Millisecond)}
	}
	x := netip.AddrPortFrom(pubIP(5, false), 4672)
	ok, _ := a.add(incoming{from: x}, pendSource, &entry{key: RandomID(), ip: x.Addr(), tcpPort: 1, udpPort: 1,
		tags: []proto.Tag{IntTag(TagSourceType, 1)}}, fn.now.Add(time.Second))
	if !ok || a.pending[x.Addr()] == nil || len(a.pending) != maxPendingIPs {
		t.Fatalf("not admitted (%d waiting)", len(a.pending))
	}
	if a.pending[pubIP(10000, false)] != nil {
		t.Fatal("not the oldest that made room")
	}
}

// Weak publishes never push out or refresh proven ones.
func TestWeakNeverDisplacesStrong(t *testing.T) {
	x := newIndex()
	first := kwEntry("Quiet Orchard.mkv", 10, pubIP(1, false))
	x.addKeyword(first, t0)
	for i := 2; i <= maxPublishersPerEntry; i++ {
		e := kwEntry("Quiet Orchard.mkv", 10, pubIP(i, false))
		e.source = first.source
		x.addKeyword(e, t0)
	}
	for i := range 50 {
		e := kwEntry("Quiet Orchard.mkv", 10, pubIP(1000+i, false))
		e.source, e.weak = first.source, true
		x.addKeyword(e, t0)
	}
	got := x.keywords[first.key][first.source][0]
	strong := 0
	for _, p := range got.publishers {
		if !p.weak {
			strong++
		}
	}
	if strong != maxPublishersPerEntry || len(got.publishers) != maxPublishersPerEntry {
		t.Fatalf("%d proven publishers left of %d", strong, len(got.publishers))
	}
	// A weak publish from a proven publisher's address keeps nothing alive.
	before := got.expires
	e := kwEntry("Quiet Orchard.mkv", 10, pubIP(1, false))
	e.source, e.weak = first.source, true
	e.expires = t0.Add(100 * time.Hour)
	x.addKeyword(e, t0.Add(time.Hour))
	if x.keywords[first.key][first.source][0].expires != before {
		t.Fatal("weak publish refreshed a proven entry")
	}
	// Sources: a weak one does not replace a proven one of its address,
	// nor push out proven ones from a full list.
	file := RandomID()
	src := func(i int, weak bool) *entry {
		return &entry{key: file, source: RandomID(), ip: pubIP(i, false), tcpPort: 1, udpPort: 1, weak: weak,
			tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(time.Hour)}
	}
	x.addSource(src(1, false))
	x.addSource(src(1, true))
	if x.sources[file][0].weak {
		t.Fatal("weak source replaced a proven one")
	}
	for i := 2; i <= maxSourcesPerFile; i++ {
		x.addSource(src(i, false))
	}
	if ok, _ := x.addSource(src(5000, true)); ok {
		t.Fatal("weak source pushed out a proven one")
	}
	if ok, _ := x.addSource(src(5001, false)); !ok {
		t.Fatal("proven source refused when full")
	}
}
