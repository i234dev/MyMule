package kad

import (
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mymule/internal/proto"
)

// Trees with more terms than any real search are refused (each term costs
// a pass over every entry).
func TestSearchTreeTermLimit(t *testing.T) {
	str := func() []byte {
		var w proto.Writer
		w.U8(seString)
		w.Str16("a")
		return w.B
	}
	var wide func(n int) []byte // a balanced OR tree with n leaves
	wide = func(n int) []byte {
		if n == 1 {
			return str()
		}
		return append(append([]byte{seBool, 1}, wide(n/2)...), wide(n-n/2)...)
	}
	if _, err := parseTree(proto.NewReader(wide(32)), 0); err != nil {
		t.Fatalf("32 leaves refused: %v", err)
	}
	if _, err := parseTree(proto.NewReader(wide(1000)), 0); err == nil {
		t.Fatal("1000 leaves accepted")
	}
}

// Answer packets stay small: 8 KB at most, and one packet of 1200 bytes to
// an address that did not prove itself.
func TestSearchAnswerBytes(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, b := storeNode(t, key)
	big := strings.Repeat("x", 200)
	for i := 0; i < 300; i++ {
		e := kwEntry("Quiet Orchard "+big+".mkv", uint64(1+i), pubIP(i, false),
			StrTag(0x80, big), StrTag(0x81, big), StrTag(0x82, big), StrTag(0x83, big))
		a.index.addKeyword(e, fn.now)
	}
	var q proto.Writer
	WriteID(&q, key)
	q.U16(0)
	for _, proven := range []bool{false, true} {
		fn.all = nil
		rk := uint32(0)
		if proven {
			rk = proof(a, b)
		}
		b.mu.Lock()
		b.send(addrOf(a), OpSearchKeyReq, q.B, a.ID(), rk)
		b.mu.Unlock()
		fn.pump()
		packets := 0
		for _, d := range fn.all {
			if d.from != addrOf(a) {
				continue
			}
			limit := maxAnswerBytes + 100
			if !proven {
				limit = maxUnprovenAnswer + 100
			}
			if len(d.b) > limit {
				t.Fatalf("proven %v: datagram of %d bytes", proven, len(d.b))
			}
			packets++
		}
		if !proven && packets != 1 {
			t.Fatalf("%d packets to an unproven address", packets)
		}
		if proven && packets < 10 {
			t.Fatalf("only %d packets to a proven address", packets)
		}
	}
}

// Stored bytes are bounded: big tags are cut or dropped, long names cut.
func TestEntryBytes(t *testing.T) {
	huge := strings.Repeat("y", 4000)
	tags := keepTags([]proto.Tag{StrTag(0x80, huge), {ID: 0x81, Type: proto.TagBsob, Bytes: make([]byte, 255)},
		StrTag(0x82, huge), StrTag(0x83, huge), StrTag(0x84, huge), StrTag(0x85, huge)})
	n := 0
	for _, tg := range tags {
		n += len(tg.Str) + len(tg.Bytes)
		if len(tg.Str) > maxNameBytes {
			t.Fatal("long string kept")
		}
	}
	if n > maxEntryTagBytes {
		t.Fatalf("%d tag bytes kept", n)
	}
	if cut := keepTags([]proto.Tag{StrTag(0x86, strings.Repeat("z", 300))}); len(cut) != 1 || len(cut[0].Str) != maxNameBytes {
		t.Fatalf("a 300-byte string kept as %d bytes", len(cut[0].Str))
	}
	x := newIndex()
	e := kwEntry(strings.Repeat("é", 400), 5, pubIP(1, false))
	x.addKeyword(e, t0)
	for _, list := range x.keywords[e.key] {
		if name := list[0].name(); len(name) > maxNameBytes || !utf8.ValidString(name) {
			t.Fatalf("name of %d bytes", len(name))
		}
	}
	// The whole index has a byte budget.
	x.bytes = maxIndexBytes - 10
	if ok, _ := x.addKeyword(kwEntry("Quiet Orchard.mkv", 9, pubIP(2, false)), t0); ok {
		t.Fatal("added past the byte budget")
	}
	if ok, _ := x.addSource(&entry{key: RandomID(), ip: pubIP(3, false), tcpPort: 1, udpPort: 1,
		tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(time.Hour)}); ok {
		t.Fatal("source added past the byte budget")
	}
	// Accounting: what is added and then cleaned adds up to nothing.
	y := newIndex()
	for i := range 50 {
		y.addKeyword(kwEntry("Quiet Orchard.mkv", uint64(1+i%3), pubIP(i, false)), t0)
		y.addSource(&entry{key: ID{byte(i % 5)}, ip: pubIP(i, false), tcpPort: 1, udpPort: 1,
			tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(time.Hour)})
	}
	if y.bytes <= 0 {
		t.Fatal("nothing counted")
	}
	y.clean(t0.Add(48 * time.Hour))
	if y.bytes != 0 {
		t.Fatalf("%d bytes left after cleaning everything", y.bytes)
	}
}

// A publish without our key is kept only once its address answers our
// ping: a forged address never gets in.
func TestUnprovenPublishWaitsForPing(t *testing.T) {
	file := RandomID()
	fn, a, b := storeNode(t, file)
	publish := func(from netip.AddrPort) {
		var w proto.Writer
		WriteID(&w, file)
		WriteID(&w, RandomID())
		WriteTags(&w, []proto.Tag{IntTag(TagSourceType, 1), IntTag(TagSourcePort, 4662)})
		fn.inject(from, a, rawPacket(OpPublishSourceReq, w.B))
	}
	// Forged: from an address without a node, which never answers.
	victim := netip.AddrPortFrom(pubIP(77, false), 4672)
	publish(victim)
	if len(a.index.sources[file]) != 0 {
		t.Fatal("unproven source stored at once")
	}
	pinged := false
	for _, d := range fn.sent[victim] {
		if p, err := DecodePacket(d); err == nil && p.Opcode == OpPing {
			pinged = true
		}
	}
	if !pinged {
		t.Fatal("the address was not pinged")
	}
	fn.now = fn.now.Add(5 * time.Minute)
	a.prunePending(fn.now)
	if len(a.index.sources[file]) != 0 || len(a.pending) != 0 {
		t.Fatal("forged source stored")
	}
	// A real node at its address answers the ping, and is stored.
	publish(addrOf(b))
	if len(a.index.sources[file]) != 1 || a.index.sources[file][0].ip != addrOf(b).Addr() {
		t.Fatalf("%d sources after the ping was answered", len(a.index.sources[file]))
	}
	// At most a few waiting per address.
	from := netip.AddrPortFrom(pubIP(78, false), 4672)
	for i := range maxPendingPerIP + 5 {
		ok, _ := a.add(incoming{from: from}, pendSource, &entry{key: file, ip: from.Addr(), tcpPort: 1, udpPort: 1,
			tags: []proto.Tag{IntTag(TagSourceType, 1)}}, fn.now)
		if ok != (i < maxPendingPerIP) {
			t.Fatalf("publish %d waiting: %v", i, ok)
		}
	}
}

// Without knowing that our UDP port is reachable, nothing is stored.
func TestNoStoreWhenUnreachable(t *testing.T) {
	file := RandomID()
	fn, a, b := storeNode(t, file)
	a.udp.verified = false
	var w proto.Writer
	WriteID(&w, file)
	WriteID(&w, RandomID())
	WriteTags(&w, []proto.Tag{IntTag(TagSourceType, 1), IntTag(TagSourcePort, 4662)})
	b.mu.Lock()
	b.send(addrOf(a), OpPublishSourceReq, w.B, a.ID(), proof(a, b))
	b.mu.Unlock()
	fn.pump()
	if len(a.index.sources[file]) != 0 || len(sent(fn, a, b, OpPublishRes)) != 0 {
		t.Fatal("stored while unreachable")
	}
}

// One source per IP and file; one file with ever new sizes stays small.
func TestIndexPerIPAndSizes(t *testing.T) {
	x := newIndex()
	file := RandomID()
	for p := range 50 {
		x.addSource(&entry{key: file, source: RandomID(), ip: pubIP(1, false), tcpPort: uint16(1 + p), udpPort: uint16(100 + p),
			tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(time.Hour)})
	}
	if len(x.sources[file]) != 1 {
		t.Fatalf("%d sources from one IP", len(x.sources[file]))
	}
	src := RandomID()
	for i := range 20 {
		e := kwEntry("Quiet Orchard.mkv", uint64(1+i), pubIP(i, false))
		e.source = src
		x.addKeyword(e, t0)
	}
	if n := len(x.keywords[KeywordID("orchard")][src]); n != maxSizesPerFile {
		t.Fatalf("%d sizes of one file", n)
	}
}

// Pages of keyword results neither repeat nor miss entries.
func TestKeywordPages(t *testing.T) {
	x := newIndex()
	for i := range 120 {
		x.addKeyword(kwEntry("Quiet Orchard.mkv", uint64(1+i), pubIP(i, false)), t0)
	}
	seen := map[*entry]bool{}
	for start := 0; start < 120; start += 50 {
		for _, e := range x.keywordResults(KeywordID("orchard"), nil, start, 50, t0) {
			if seen[e] {
				t.Fatal("entry on two pages")
			}
			seen[e] = true
		}
	}
	if len(seen) != 120 {
		t.Fatalf("%d entries over the pages", len(seen))
	}
}

// When a keyword publish cannot start, the rotation does not move on.
func TestRotationKeptOnFailedStart(t *testing.T) {
	fn := newFakeNet()
	var shared []SharedFile
	for i := range 200 {
		shared = append(shared, SharedFile{Hash: [16]byte{byte(i), 9}, Name: "Harbor.avi", Size: 5, Complete: true})
	}
	p := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, Shared: func() (int, []SharedFile) { return 1, shared }})
	o := fn.node(t, 2, Config{})
	p.table.Add(contactOf(o), false, false, t0)
	p.table.SetAlive(o.ID(), t0)
	p.pub.allowed, p.firewalledN, p.lastContact = true, 2, fn.now
	p.refreshShared()
	k := p.pub.byWord["harbor"]
	p.searches[k.id] = newSearch(searchKeyword, k.id, fn.now) // busy with that ID
	p.tickPublish(fn.now)
	if k.offset != 0 {
		t.Fatalf("offset moved to %d without a publish", k.offset)
	}
}

// One publisher republishing under ever new names adds no names.
func TestOneNamePerPublisher(t *testing.T) {
	x := newIndex()
	src := RandomID()
	for i := range 50 {
		e := kwEntry("Quiet Orchard "+strings.Repeat("v", i)+".mkv", 10, pubIP(1, false))
		e.source = src
		x.addKeyword(e, t0.Add(time.Duration(i)*time.Minute))
	}
	got := x.keywords[KeywordID("orchard")][src][0]
	if len(got.names) != 1 {
		t.Fatalf("%d names from one publisher", len(got.names))
	}
}
