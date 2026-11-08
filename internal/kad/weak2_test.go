package kad

import (
	"net/netip"
	"strconv"
	"strings"
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
	// One packet's worth waits per address, no more: a second is refused.
	fn.inject(x, a, rawPacket(OpPublishKeyReq, w.B))
	if n := len(a.pending[x.Addr()].items); n != filesPerPublish {
		t.Fatalf("%d waiting after a second packet", n)
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

// What waits is limited in all, in entries and bytes, whatever the
// number of addresses: forged ones cost nothing. Full, new publishes are
// refused (not stored, not pinged); expired ones make room again, and
// proven publishes still go in at once.
func TestPendingBudget(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, b := storeNode(t, key)
	big := func(ip netip.Addr, i int) *entry {
		e := &entry{key: key, source: RandomID(), ip: ip, size: uint64(1 + i), expires: fn.now.Add(time.Hour)}
		e.names = []fileName{{name: strings.Repeat("n", maxNameBytes), popularity: 1}}
		var tags []proto.Tag
		for j := range maxTagsPerEntry {
			tags = append(tags, proto.Tag{Name: "t" + strconv.Itoa(j), Type: proto.TagString, Str: strings.Repeat("v", 20)})
		}
		e.tags = keepTags(tags)
		return e
	}
	at := fn.now
	for i := range maxPendingIPs {
		from := netip.AddrPortFrom(pubIP(20000+i, false), 4672)
		at = at.Add(time.Millisecond)
		for j := range maxPendingPerIP {
			if ok, _ := a.add(incoming{from: from}, pendKeyword, big(from.Addr(), j), at); !ok {
				t.Fatalf("publish %d/%d refused", i, j)
			}
		}
	}
	if a.pendingItems > maxPendingItems || a.pendingBytes > maxPendingBytes || a.pendingItems == 0 {
		t.Fatalf("%d entries, %d bytes waiting", a.pendingItems, a.pendingBytes)
	}
	if a.pending[pubIP(20000, false)] != nil || a.pending[pubIP(20000+maxPendingIPs-1, false)] == nil {
		t.Fatal("not the oldest that made room")
	}
	sum, items := 0, 0
	for _, p := range a.pending {
		sum += p.bytes
		items += len(p.items)
	}
	if sum != a.pendingBytes || items != a.pendingItems {
		t.Fatalf("totals %d/%d, counted %d/%d", a.pendingBytes, a.pendingItems, sum, items)
	}
	// Full: a new address still gets in (and is pinged), in the place of
	// what waited longest.
	late := netip.AddrPortFrom(pubIP(9, false), 4672)
	ok, _ := a.add(incoming{from: late}, pendKeyword, big(late.Addr(), 0), at)
	pinged := false
	for _, q := range fn.queue {
		pinged = pinged || q.to == late
	}
	if !ok || a.pending[late.Addr()] == nil || !pinged {
		t.Fatalf("while full: taken %v, pinged %v", ok, pinged)
	}
	if a.pendingItems > maxPendingItems || a.pendingBytes > maxPendingBytes {
		t.Fatalf("over the budget: %d entries, %d bytes", a.pendingItems, a.pendingBytes)
	}
	// Proven publishes are not held back.
	if ok, _ := a.add(incoming{from: addrOf(b), validKey: true}, pendKeyword, kwEntry("Quiet Orchard.mkv", 7, addrOf(b).Addr()), fn.now); !ok {
		t.Fatal("proven publish refused")
	}
	// Expired, they all go, and the totals go back to nothing.
	a.prunePending(at.Add(pendingWait + time.Second))
	if a.pendingItems != 0 || a.pendingBytes != 0 || len(a.pending) != 0 {
		t.Fatalf("left: %d entries, %d bytes", a.pendingItems, a.pendingBytes)
	}
}

// A proven address takes what waits for it off the totals.
func TestPendingBudgetProved(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, _ := storeNode(t, key)
	x := netip.AddrPortFrom(pubIP(91, false), 4672)
	for i := range 3 {
		a.add(incoming{from: x}, pendKeyword, kwEntry("Quiet Orchard.mkv", uint64(1+i), x.Addr()), fn.now)
	}
	if a.pendingItems != 3 || a.pendingBytes <= 0 {
		t.Fatalf("%d entries, %d bytes", a.pendingItems, a.pendingBytes)
	}
	a.proved(x.Addr(), fn.now)
	if a.pendingItems != 0 || a.pendingBytes != 0 {
		t.Fatalf("after proof: %d entries, %d bytes", a.pendingItems, a.pendingBytes)
	}
	// One that waited too long is dropped, not kept, when its address
	// publishes again.
	a.add(incoming{from: x}, pendKeyword, kwEntry("Quiet Orchard.mkv", 1, x.Addr()), fn.now)
	a.add(incoming{from: x}, pendKeyword, kwEntry("Quiet Orchard.mkv", 2, x.Addr()), fn.now.Add(pendingWait+time.Second))
	if a.pendingItems != 1 || len(a.pending[x.Addr()].items) != 1 {
		t.Fatalf("%d entries waiting", a.pendingItems)
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
