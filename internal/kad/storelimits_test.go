package kad

import (
	"net/netip"
	"strconv"
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

// One address, or one /24, creates a limited number of keyword entries;
// others still get in.
func TestKeywordEntriesPerOwner(t *testing.T) {
	x := newIndex()
	key := KeywordID("orchard")
	add := func(ip netip.Addr, i int) bool {
		e := kwEntry("Quiet Orchard "+strconv.Itoa(i)+".mkv", uint64(1+i), ip)
		e.key = key
		ok, _ := x.addKeyword(e, t0)
		return ok
	}
	one := netip.AddrFrom4([4]byte{5, 6, 7, 1})
	taken := 0
	for i := range maxEntriesPerIP + 100 {
		if add(one, i) {
			taken++
		}
	}
	if taken != maxEntriesPerIP {
		t.Fatalf("one address created %d entries", taken)
	}
	// Its refreshes still go in.
	first := x.keywords[key]
	var any *entry
	for _, l := range first {
		any = l[0]
		break
	}
	again := kwEntry(any.name(), any.size, one)
	again.key, again.source = key, any.source
	if ok, _ := x.addKeyword(again, t0.Add(time.Hour)); !ok {
		t.Fatal("refresh refused")
	}
	// The rest of its /24 together: maxEntriesPerSubnet.
	for host := byte(2); host < 10; host++ {
		for i := range maxEntriesPerIP {
			if add(netip.AddrFrom4([4]byte{5, 6, 7, host}), 10000*int(host)+i) {
				taken++
			}
		}
	}
	if taken != maxEntriesPerSubnet {
		t.Fatalf("one /24 created %d entries", taken)
	}
	if !add(netip.AddrFrom4([4]byte{5, 6, 8, 1}), 999999) {
		t.Fatal("another network refused")
	}
	// Expired entries are uncounted.
	x.clean(t0.Add(keywordLifetime + time.Hour))
	if len(x.ownIPs) != 0 || len(x.ownSubnets) != 0 || x.nKeyword != 0 {
		t.Fatalf("after expiry: %d owners, %d entries", len(x.ownIPs), x.nKeyword)
	}
}

// A full index makes room for a proven entry from a network that
// publishes little by dropping one nobody vouches for; weak entries, and
// entries from a network that publishes much, never push others out.
func TestFullIndexEvictsUntrusted(t *testing.T) {
	x := newIndex()
	key := KeywordID("orchard")
	n, owner := 0, 0
	fill := func(key ID, owners int) {
		for range owners {
			owner++
			for h := range maxEntriesPerSubnet / maxEntriesPerIP {
				ip := netip.AddrFrom4([4]byte{20, byte(owner), 0, byte(h)})
				for range maxEntriesPerIP {
					n++
					e := kwEntry("Flood "+strconv.Itoa(n)+".mkv", uint64(n), ip)
					e.key = key
					if ok, _ := x.addKeyword(e, t0); !ok {
						t.Fatalf("flood entry %d refused", n)
					}
				}
			}
		}
	}
	// One keyword full.
	fill(key, maxFilesPerKeyword/maxEntriesPerSubnet)
	if len(x.keywords[key]) != maxFilesPerKeyword {
		t.Fatalf("%d files", len(x.keywords[key]))
	}
	honest := netip.AddrFrom4([4]byte{30, 1, 1, 1})
	e := kwEntry("Quiet Orchard.mkv", 7, honest)
	e.key = key
	if ok, _ := x.addKeyword(e, t0); !ok || len(x.keywords[key]) != maxFilesPerKeyword {
		t.Fatalf("honest entry in a full keyword: %v, %d files", ok, len(x.keywords[key]))
	}
	weak := kwEntry("Quiet Orchard 2.mkv", 8, netip.AddrFrom4([4]byte{30, 2, 1, 1}))
	weak.key, weak.weak = key, true
	if ok, _ := x.addKeyword(weak, t0); ok {
		t.Fatal("weak entry pushed another out")
	}
	flooder := kwEntry("Flood again.mkv", 9, netip.AddrFrom4([4]byte{20, 1, 0, 9}))
	flooder.key = key
	if ok, _ := x.addKeyword(flooder, t0); ok {
		t.Fatal("entry from the flooding network pushed another out")
	}
	// The whole index full, over other keywords.
	other := KeywordID("harbor")
	fill(other, (maxKeywordEntries-maxFilesPerKeyword)/maxEntriesPerSubnet)
	if x.nKeyword != maxKeywordEntries {
		t.Fatalf("%d entries", x.nKeyword)
	}
	e2 := kwEntry("Blue Harbor.mkv", 7, netip.AddrFrom4([4]byte{31, 1, 1, 1}))
	e2.key = KeywordID("comet")
	if ok, _ := x.addKeyword(e2, t0); !ok || x.nKeyword != maxKeywordEntries {
		t.Fatalf("honest entry in a full index: %v, %d entries", ok, x.nKeyword)
	}
	// The counts stay right.
	owned := 0
	for _, c := range x.ownIPs {
		owned += c
	}
	if owned != x.nKeyword {
		t.Fatalf("%d owned, %d entries", owned, x.nKeyword)
	}
}

// The entry dropped to make room may be the last of the new entry's own
// keyword: the new one still lands in the index.
func TestEvictLastOfKeyword(t *testing.T) {
	x := newIndex()
	w := kwEntry("Quiet Orchard.mkv", 7, netip.AddrFrom4([4]byte{40, 1, 1, 1}))
	w.weak = true
	if ok, _ := x.addKeyword(w, t0); !ok {
		t.Fatal("weak entry refused")
	}
	x.nKeyword = maxKeywordEntries // full (of what we pretend is there)
	e := kwEntry("Quiet Orchard.mkv", 8, netip.AddrFrom4([4]byte{41, 1, 1, 1}))
	if ok, _ := x.addKeyword(e, t0); !ok {
		t.Fatal("not taken")
	}
	got := x.keywords[e.key][e.source]
	if len(got) != 1 || got[0] != e || x.keywords[w.key][w.source] != nil {
		t.Fatalf("index: %v", x.keywords[e.key])
	}
}

// A search with a large tree over a large keyword stops after
// searchWork: asked for results far down the list, it gives none. The
// files' order is kept between searches and follows new files.
func TestKeywordSearchWork(t *testing.T) {
	x := newIndex()
	key := KeywordID("orchard")
	for i := range 20000 {
		e := kwEntry(strings.Repeat("quiet orchard ", 18)+strconv.Itoa(i)+".mkv", uint64(1+i), pubIP(i, false))
		e.key = key
		x.addKeyword(e, t0)
	}
	// ORs, as deep as allowed, of strings that never match, and last one
	// that always does.
	var w proto.Writer
	for range maxTreeDepth - 1 {
		w.U8(seBool)
		w.U8(1)
		w.U8(seString)
		w.U16(5)
		w.Bytes([]byte("zzzzz"))
	}
	w.U8(seString)
	w.U16(5)
	w.Bytes([]byte("quiet"))
	tr := tree(t, w.B)
	if res := x.keywordResults(key, tr, 0, 50, t0); len(res) != 50 {
		t.Fatalf("%d results at the start", len(res))
	}
	if res := x.keywordResults(key, tr, 19000, 300, t0); len(res) != 0 {
		t.Fatalf("%d results past the work limit", len(res))
	}
	var work int
	tr.matchesWork(x.keywords[key][x.sorted[key][0]][0], &work)
	if work*len(x.sorted[key]) <= searchWork {
		t.Fatalf("the test does not reach the limit (%d per entry)", work)
	}
	// Without a tree every entry is looked at.
	if res := x.keywordResults(key, nil, 0, 300, t0); len(res) != 300 {
		t.Fatalf("%d results without a tree", len(res))
	}
	ids := x.sorted[key]
	if len(ids) != 20000 {
		t.Fatalf("order not kept: %d", len(ids))
	}
	e := kwEntry("Blue Harbor.mkv", 5, pubIP(1, true))
	e.key = key
	x.addKeyword(e, t0)
	if _, ok := x.sorted[key]; ok {
		t.Fatal("order kept after a new file")
	}
	x.keywordResults(key, nil, 0, 300, t0)
	if len(x.sorted[key]) != 20001 {
		t.Fatalf("%d files in order", len(x.sorted[key]))
	}
}

// Keyword searches without our key share a work budget per second;
// cheap ones barely use it, and searches with our key are not held back.
func TestKeywordSearchesPerSecond(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, _ := storeNode(t, key)
	a.index.addKeyword(kwEntry("Quiet Orchard.mkv", 7, pubIP(3, false)), fn.now)
	var w proto.Writer
	WriteID(&w, key)
	w.U16(0)
	ask := func(i int, keyed bool) bool {
		x := netip.AddrPortFrom(pubIP(600+i, false), 4672)
		d := rawPacket(OpSearchKeyReq, w.B)
		if keyed {
			d = (&Keys{Secret: [32]byte{4}}).EncryptKad(d, addrOf(a).Addr(), ID{}, a.keys.VerifyKey(x.Addr()))
		}
		fn.inject(x, a, d)
		return len(fn.sent[x]) > 0
	}
	for i := range 30 {
		if !ask(i, false) {
			t.Fatalf("cheap search %d not answered", i)
		}
	}
	a.kwWork = searchWorkPerSecond // spent
	if ask(100, false) {
		t.Fatal("answered beyond the budget")
	}
	if !ask(101, true) {
		t.Fatal("search with our key held back")
	}
	fn.now = fn.now.Add(time.Second)
	if !ask(102, false) {
		t.Fatal("not answered a second later")
	}
}
