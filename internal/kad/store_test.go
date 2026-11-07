package kad

import (
	"crypto/rand"
	"net/netip"
	"strings"
	"testing"
	"time"

	"mymule/internal/proto"
)

func kwEntry(name string, size uint64, ip netip.Addr, tags ...proto.Tag) *entry {
	e := &entry{key: KeywordID("orchard"), source: RandomID(), ip: ip, udpPort: 4672, size: size,
		names: []fileName{{name: name, popularity: 1}}, tags: keepTags(tags), expires: t0.Add(keywordLifetime)}
	e.settle()
	return e
}

func tree(t *testing.T, b []byte) *term {
	t.Helper()
	tr, err := parseTree(proto.NewReader(b), 0)
	if err != nil {
		t.Fatalf("parse % X: %v", b, err)
	}
	return tr
}

func TestSearchTreeMatches(t *testing.T) {
	e := kwEntry("Quiet.Orchard.2019.MKV", 5000, pubIP(1, false), StrTag(TagFileType, "Video"), IntTag(TagMediaBitrate, 900))
	str := func(s string) []byte {
		var w proto.Writer
		w.U8(seString)
		w.Str16(s)
		return w.B
	}
	meta := func(id byte, v string) []byte {
		var w proto.Writer
		w.U8(seMetaStr)
		w.Str16(v)
		w.U16(1)
		w.U8(id)
		return w.B
	}
	num := func(id byte, cmp byte, v uint64, wide bool) []byte {
		var w proto.Writer
		if wide {
			w.U8(seNumeric64)
			w.U64(v)
		} else {
			w.U8(seNumeric)
			w.U32(uint32(v))
		}
		w.U8(cmp)
		w.U16(1)
		w.U8(id)
		return w.B
	}
	op := func(o byte, l, r []byte) []byte { return append(append([]byte{seBool, o}, l...), r...) }
	for _, tc := range []struct {
		name string
		b    []byte
		want bool
	}{
		{"words", str("quiet 2019"), true},
		{"missing word", str("quiet harbor"), false},
		{"case", str("QUIET"), true},
		{"type", meta(TagFileType, "video"), true},
		{"other type", meta(TagFileType, "Audio"), false},
		{"extension", meta(TagFileFormat, "mkv"), true},
		{"other extension", meta(TagFileFormat, "avi"), false},
		{"size >=", num(TagFileSize, 3, 5000, false), true},
		{"size >", num(TagFileSize, 1, 5000, false), false},
		{"size <", num(TagFileSize, 2, 5001, true), true},
		{"size =", num(TagFileSize, 0, 5000, false), true},
		{"size <>", num(TagFileSize, 5, 5000, false), false},
		{"bitrate <=", num(TagMediaBitrate, 4, 899, false), false},
		{"missing tag", num(TagMediaLength, 3, 0, false), false},
		{"and", op(0, str("quiet"), meta(TagFileType, "Video")), true},
		{"or", op(1, str("harbor"), str("orchard")), true},
		{"not", op(2, str("quiet"), str("harbor")), true},
		{"not excluded", op(2, str("quiet"), str("orchard")), false},
	} {
		if got := tree(t, tc.b).matches(e); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// Hostile trees. A well-formed tree nested deeper than eMule allows:
	var deepTree func(n int) []byte
	deepTree = func(n int) []byte {
		if n == 0 {
			return str("a")
		}
		return op(0, deepTree(n-1), str("a"))
	}
	if _, err := parseTree(proto.NewReader(deepTree(maxTreeDepth-1)), 0); err != nil {
		t.Fatalf("tree at the depth limit refused: %v", err)
	}
	deep := deepTree(maxTreeDepth + 1)
	for name, b := range map[string][]byte{
		"too deep": deep, "bad operator": {seBool, 7}, "bad comparison": num(TagFileSize, 9, 1, false),
		"unknown term": {0x42}, "truncated": str("quiet")[:4], "empty": {}, "half an and": op(0, str("a"), nil),
	} {
		if _, err := parseTree(proto.NewReader(b), 0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestIndexKeywordMerge(t *testing.T) {
	x := newIndex()
	a, b := pubIP(1, false), pubIP(2, false)
	e1 := kwEntry("Quiet Orchard.mkv", 100, a, StrTag(TagFileType, "Video"))
	if ok, load := x.addKeyword(e1, t0); !ok || load != 1 {
		t.Fatalf("first: %v %d", ok, load)
	}
	// Another publisher, another name, same file and size: merged.
	e2 := kwEntry("quiet orchard (copy).mkv", 100, b, StrTag(TagFileType, "Video"))
	e2.source = e1.source
	x.addKeyword(e2, t0.Add(time.Minute))
	list := x.keywords[e1.key][e1.source]
	if len(list) != 1 || x.nKeyword != 1 || len(list[0].publishers) != 2 || len(list[0].names) != 2 {
		t.Fatalf("merge: %d entries, %d publishers, %d names", len(list), len(list[0].publishers), len(list[0].names))
	}
	// A in time republishes its name: more popular; the name returned
	// becomes the most published one.
	e3 := kwEntry("Quiet Orchard.mkv", 100, a, StrTag(TagFileType, "Video"))
	e3.source = e1.source
	x.addKeyword(e3, t0.Add(sourceLifetime))
	got := x.keywords[e1.key][e1.source][0]
	if got.name() != "Quiet Orchard.mkv" || len(got.publishers) != 2 {
		t.Fatalf("name %q, %d publishers", got.name(), len(got.publishers))
	}
	for _, n := range got.names {
		if n.name == "Quiet Orchard.mkv" && n.popularity != 2 {
			t.Fatalf("popularity %d after a republish in time", n.popularity)
		}
	}
	// Two publishers of different /24s: trusted.
	if tr := x.trust(got, t0); tr < 1 {
		t.Fatalf("trust %v", tr)
	}
	// Another size: its own entry.
	e4 := kwEntry("Quiet Orchard.mkv", 200, a)
	e4.source = e1.source
	x.addKeyword(e4, t0)
	if len(x.keywords[e1.key][e1.source]) != 2 || x.nKeyword != 2 {
		t.Fatal("other size merged")
	}
	// Full: refused, with load 100.
	full := newIndex()
	full.nKeyword = maxKeywordEntries + 1
	if ok, load := full.addKeyword(kwEntry("Quiet Orchard.mkv", 1, a), t0); ok || load != 100 {
		t.Fatalf("full index: %v %d", ok, load)
	}
	// Without name or size: refused.
	if ok, _ := x.addKeyword(kwEntry("", 5, a), t0); ok {
		t.Fatal("no name accepted")
	}
	if ok, _ := x.addKeyword(kwEntry("x", 0, a), t0); ok {
		t.Fatal("no size accepted")
	}
}

// Many publishers from one network count little.
func TestIndexTrust(t *testing.T) {
	x := newIndex()
	e := kwEntry("Blue Harbor.avi", 10, pubIP(1, true))
	x.addKeyword(e, t0)
	for i := 2; i <= 30; i++ {
		o := kwEntry("Blue Harbor.avi", 10, pubIP(i, true))
		o.source = e.source
		x.addKeyword(o, t0)
	}
	got := x.keywords[e.key][e.source][0]
	if len(got.publishers) != 30 || x.trust(got, t0) > 10.01 {
		t.Fatalf("%d publishers, trust %v", len(got.publishers), x.trust(got, t0))
	}
	for i := 1; i <= 120; i++ {
		o := kwEntry("Blue Harbor.avi", 10, pubIP(1000+i, false))
		o.source = e.source
		x.addKeyword(o, t0)
	}
	if got := x.keywords[e.key][e.source][0]; len(got.publishers) != maxPublishersPerEntry {
		t.Fatalf("%d publishers kept", len(got.publishers))
	}
	total := 0
	for _, c := range x.pubSubnets {
		total += c
	}
	if total != maxPublishersPerEntry {
		t.Fatalf("subnet counts add to %d", total)
	}
}

func TestIndexSourcesAndNotes(t *testing.T) {
	x := newIndex()
	file := RandomID()
	src := func(ip netip.Addr, tcp, udp uint16) *entry {
		return &entry{key: file, source: RandomID(), ip: ip, tcpPort: tcp, udpPort: udp, tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(sourceLifetime)}
	}
	x.addSource(src(pubIP(1, false), 4662, 4672))
	x.addSource(src(pubIP(1, false), 4662, 9999)) // same client: replaces
	if len(x.sources[file]) != 1 || x.nSource != 1 || x.sources[file][0].udpPort != 9999 {
		t.Fatalf("%d sources", len(x.sources[file]))
	}
	if ok, _ := x.addSource(src(pubIP(2, false), 0, 4672)); ok {
		t.Fatal("source without TCP port")
	}
	for i := 10; i < 10+maxSourcesPerFile+5; i++ {
		x.addSource(src(pubIP(i, false), 4662, 4672))
	}
	if len(x.sources[file]) != maxSourcesPerFile {
		t.Fatalf("%d sources kept", len(x.sources[file]))
	}
	if x.sources[file][0].ip != pubIP(10+maxSourcesPerFile+4, false) {
		t.Fatal("newest not first")
	}
	// Notes: one per client.
	note := func(ip netip.Addr, id ID) *entry {
		return &entry{key: file, source: id, ip: ip, names: []fileName{{name: "n", popularity: 1}}, expires: t0.Add(notesLifetime)}
	}
	id := RandomID()
	x.addNote(note(pubIP(1, false), id))
	x.addNote(note(pubIP(2, false), id))
	if len(x.notes[file]) != 1 || x.notes[file][0].ip != pubIP(2, false) {
		t.Fatalf("%d notes", len(x.notes[file]))
	}
	x.addNote(note(pubIP(2, false), RandomID())) // same IP: replaces
	if len(x.notes[file]) != 1 || x.nNotes != 1 {
		t.Fatalf("%d notes", len(x.notes[file]))
	}
	// Expired entries go.
	x.clean(t0.Add(25 * time.Hour))
	if x.nSource != 0 || x.nNotes != 0 || len(x.sources) != 0 || len(x.notes) != 0 {
		t.Fatalf("after clean: %d sources, %d notes", x.nSource, x.nNotes)
	}
}

// The totals stop growth whatever keys are used.
func TestIndexTotals(t *testing.T) {
	x := newIndex()
	for i := 0; i < maxKeys+10; i++ {
		e := &entry{key: RandomID(), source: RandomID(), ip: pubIP(i, false), tcpPort: 1, udpPort: 1, tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(time.Hour)}
		x.addSource(e)
	}
	if len(x.sources) != maxKeys {
		t.Fatalf("%d keys", len(x.sources))
	}
	for i := 0; i < maxKeys+10; i++ {
		e := kwEntry("Quiet Orchard.mkv", 10, pubIP(i, false))
		e.key = RandomID()
		x.addKeyword(e, t0)
	}
	if len(x.keywords) != maxKeys {
		t.Fatalf("%d keyword keys", len(x.keywords))
	}
}

// storeNode returns a node whose ID is close to target, and another node
// to send it packets from.
func storeNode(t *testing.T, target ID) (*fakeNet, *Node, *Node) {
	fn := newFakeNet()
	a, b := fn.node(t, 1, Config{}), fn.node(t, 2, Config{})
	near := target
	near[15] ^= 1
	setID(a, near)
	a.udp.verified = true // reachable: it stores
	return fn, a, b
}

// proof is the receiver key a node gives with its packets to a: proof
// that it is at the address it sends from.
func proof(a, b *Node) uint32 { return a.keys.VerifyKey(addrOf(b).Addr()) }

func sent(fn *fakeNet, from, to *Node, op byte) []Packet {
	var out []Packet
	for _, d := range fn.all {
		if d.from != addrOf(from) || d.to != addrOf(to) {
			continue
		}
		r, err := to.keys.Decrypt(d.b, d.from.Addr())
		if err != nil {
			continue
		}
		if p, err := DecodePacket(r.Data); err == nil && p.Opcode == op {
			out = append(out, p)
		}
	}
	return out
}

func TestPublishKeywordAndSearch(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, b := storeNode(t, key)
	file := RandomID()
	var w proto.Writer
	WriteID(&w, key)
	w.U16(1)
	WriteID(&w, file)
	WriteTags(&w, []proto.Tag{StrTag(TagFileName, "Quiet Orchard.mkv"), IntTag(TagFileSize, 777), StrTag(TagFileType, "Video")})
	b.mu.Lock()
	b.send(addrOf(a), OpPublishKeyReq, w.B, a.ID(), proof(a, b))
	b.mu.Unlock()
	fn.pump()
	res := sent(fn, a, b, OpPublishRes)
	if len(res) != 1 || ReadID(proto.NewReader(res[0].Payload)) != key {
		t.Fatalf("%d publish answers", len(res))
	}
	if a.index.nKeyword != 1 {
		t.Fatal("not stored")
	}
	// A search for it, restricted to videos.
	var q proto.Writer
	WriteID(&q, key)
	q.U16(0x8000)
	q.U8(seMetaStr)
	q.Str16("Video")
	q.U16(1)
	q.U8(TagFileType)
	b.mu.Lock()
	b.send(addrOf(a), OpSearchKeyReq, q.B, a.ID(), 0)
	b.mu.Unlock()
	fn.pump()
	got := sent(fn, a, b, OpSearchRes)
	if len(got) != 1 {
		t.Fatalf("%d search answers", len(got))
	}
	r := proto.NewReader(got[0].Payload)
	if ReadID(r) != a.ID() || ReadID(r) != key || r.U16() != 1 || ReadID(r) != file {
		t.Fatal("answer header")
	}
	tags, err := ReadTags(r)
	if err != nil || TagStr(tags, TagFileName) != "Quiet Orchard.mkv" || TagStr(tags, TagFileType) != "Video" {
		t.Fatalf("tags %v %v", tags, err)
	}
	if v, _ := TagInt(tags, TagFileSize); v != 777 {
		t.Fatal("size")
	}
	if info, ok := TagInt(tags, TagPublishInfo); !ok || info>>16&0xFF != 1 || info>>24 != 1 {
		t.Fatalf("publish info %x", info)
	}
	// A keyword far from our ID is not stored.
	far := key
	far[0] ^= 0xFF
	var w2 proto.Writer
	WriteID(&w2, far)
	w2.U16(1)
	WriteID(&w2, file)
	WriteTags(&w2, []proto.Tag{StrTag(TagFileName, "Quiet Orchard.mkv"), IntTag(TagFileSize, 777)})
	b.mu.Lock()
	b.send(addrOf(a), OpPublishKeyReq, w2.B, a.ID(), proof(a, b))
	b.mu.Unlock()
	fn.pump()
	if a.index.nKeyword != 1 {
		t.Fatal("far keyword stored")
	}
}

// A published source is stored with the address it was sent from, not one
// it claims, and searches get it back.
func TestPublishSourceAndSearch(t *testing.T) {
	file := RandomID()
	fn, a, b := storeNode(t, file)
	var w proto.Writer
	WriteID(&w, file)
	WriteID(&w, RandomID())
	WriteTags(&w, []proto.Tag{IntTag(TagSourceIP, uint64(ipToU32(pubIP(99, false)))), IntTag(TagSourceType, 1), IntTag(TagSourcePort, 4662), IntTag(TagFileSize, 123)})
	b.mu.Lock()
	b.send(addrOf(a), OpPublishSourceReq, w.B, a.ID(), proof(a, b))
	b.mu.Unlock()
	fn.pump()
	if len(sent(fn, a, b, OpPublishRes)) != 1 {
		t.Fatal("no publish answer")
	}
	var q proto.Writer
	WriteID(&q, file)
	q.U16(0)
	q.U64(123)
	b.mu.Lock()
	b.send(addrOf(a), OpSearchSourceReq, q.B, a.ID(), 0)
	b.mu.Unlock()
	fn.pump()
	got := sent(fn, a, b, OpSearchRes)
	if len(got) != 1 {
		t.Fatalf("%d answers", len(got))
	}
	r := proto.NewReader(got[0].Payload)
	ReadID(r)
	ReadID(r)
	if r.U16() != 1 {
		t.Fatal("count")
	}
	ReadID(r)
	tags, _ := ReadTags(r)
	ips := 0
	for _, tg := range tags {
		if tg.ID == TagSourceIP {
			ips++
			if ipFromU32(uint32(tg.Int)) != addrOf(b).Addr() {
				t.Fatalf("source IP %v", ipFromU32(uint32(tg.Int)))
			}
		}
	}
	if ips != 1 {
		t.Fatalf("%d source IP tags", ips)
	}
	if p, _ := TagInt(tags, TagSourceUDPPort); p != uint64(addrOf(b).Port()) {
		t.Fatalf("UDP port %d", p)
	}
	// A publish without a source type is not a source.
	other := RandomID()
	var w3 proto.Writer
	WriteID(&w3, file)
	WriteID(&w3, other)
	WriteTags(&w3, []proto.Tag{IntTag(TagSourcePort, 4662), IntTag(TagFileSize, 123)})
	b.mu.Lock()
	b.send(addrOf(a), OpPublishSourceReq, w3.B, a.ID(), proof(a, b))
	b.mu.Unlock()
	fn.pump()
	for _, e := range a.index.sources[file] {
		if e.source == other {
			t.Fatal("publish without a source type stored")
		}
	}
	// Another size: not returned.
	var q2 proto.Writer
	WriteID(&q2, file)
	q2.U16(0)
	q2.U64(999)
	before := len(sent(fn, a, b, OpSearchRes))
	b.mu.Lock()
	b.send(addrOf(a), OpSearchSourceReq, q2.B, a.ID(), 0)
	b.mu.Unlock()
	fn.pump()
	if len(sent(fn, a, b, OpSearchRes)) != before {
		t.Fatal("source of another size returned")
	}
}

// A node that did not prove its address gets one packet of results; one
// that did, up to 300 in packets of 50.
func TestSearchAnswerSize(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, b := storeNode(t, key)
	for i := 0; i < 400; i++ {
		a.index.addKeyword(kwEntry("Quiet Orchard.mkv", uint64(1+i), pubIP(i, false)), fn.now)
	}
	var q proto.Writer
	WriteID(&q, key)
	q.U16(0)
	b.mu.Lock()
	b.send(addrOf(a), OpSearchKeyReq, q.B, a.ID(), 0)
	b.mu.Unlock()
	fn.pump()
	if n := len(sent(fn, a, b, OpSearchRes)); n != 1 {
		t.Fatalf("%d packets without proof", n)
	}
	fn.all = nil
	b.mu.Lock()
	b.send(addrOf(a), OpSearchKeyReq, q.B, a.ID(), a.keys.VerifyKey(addrOf(b).Addr()))
	b.mu.Unlock()
	fn.pump()
	got := sent(fn, a, b, OpSearchRes)
	total := 0
	for _, p := range got {
		r := proto.NewReader(p.Payload)
		ReadID(r)
		ReadID(r)
		total += int(r.U16())
	}
	if len(got) != 6 || total != maxResults {
		t.Fatalf("%d packets, %d results", len(got), total)
	}
}

// Our shared files get published: their words to the keyword's nodes, the
// files as sources to the file's nodes.
func TestPublishOurFiles(t *testing.T) {
	fn := newFakeNet()
	file := ID{0x42, 1}
	shared := []SharedFile{{Hash: file, Name: "Quiet Orchard.mkv", Size: 5000, Complete: true}}
	p := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, Shared: func() (int, []SharedFile) { return 1, shared },
		FileType: func(string) string { return "Video" }})
	kwNode := fn.node(t, 2, Config{})
	setID(kwNode, func() ID { id := KeywordID("orchard"); id[15] ^= 1; return id }())
	fileNode := fn.node(t, 3, Config{})
	setID(fileNode, func() ID { id := file; id[15] ^= 1; return id }())
	kwNode.udp.verified, fileNode.udp.verified = true, true
	for _, o := range []*Node{kwNode, fileNode} {
		p.table.Add(contactOf(o), false, false, t0)
		p.table.SetAlive(o.ID(), t0)
	}
	p.pub.allowed = true
	p.firewalledN = 2 // our TCP port is open
	p.lastContact = fn.now
	for range 60 {
		fn.now = fn.now.Add(time.Second)
		p.lastContact = fn.now
		p.mu.Lock()
		p.tick(fn.now)
		p.mu.Unlock()
		fn.pump()
	}
	if kwNode.index.nKeyword != 1 {
		t.Fatalf("keyword node has %d entries", kwNode.index.nKeyword)
	}
	for _, list := range kwNode.index.keywords[KeywordID("orchard")] {
		if e := list[0]; e.name() != "Quiet Orchard.mkv" || e.size != 5000 || TagStr(e.tags, TagFileType) != "Video" {
			t.Fatalf("entry %+v", e)
		}
	}
	srcs := fileNode.index.sources[file]
	if len(srcs) != 1 || srcs[0].ip != addrOf(p).Addr() || srcs[0].tcpPort != 4662 || srcs[0].source != ID(p.keys.UserHash) {
		t.Fatalf("sources %+v", srcs)
	}
	// "mkv" (the extension) was not published.
	if _, ok := p.pub.byWord["mkv"]; ok {
		t.Fatal("extension published as a keyword")
	}
	// Not again before 24 h (keywords) and 5 h (sources), also once the
	// first publishes have ended.
	kwReqs, srcReqs := len(sent(fn, p, kwNode, OpPublishKeyReq)), len(sent(fn, p, fileNode, OpPublishSourceReq))
	for range 400 {
		fn.now = fn.now.Add(time.Second)
		p.lastContact = fn.now
		p.mu.Lock()
		p.tick(fn.now)
		p.mu.Unlock()
		fn.pump()
	}
	if len(sent(fn, p, kwNode, OpPublishKeyReq)) != kwReqs || len(sent(fn, p, fileNode, OpPublishSourceReq)) != srcReqs {
		t.Fatal("republished early")
	}
}

// Nothing is published while firewalled, before our own lookup, or
// without shared files.
func TestPublishConditions(t *testing.T) {
	fn := newFakeNet()
	shared := []SharedFile{{Hash: ID{1}, Name: "Blue Harbor.avi", Size: 5, Complete: true}}
	p := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, Shared: func() (int, []SharedFile) { return 1, shared }})
	o := fn.node(t, 2, Config{})
	p.table.Add(contactOf(o), false, false, t0)
	p.table.SetAlive(o.ID(), t0)
	p.lastContact = fn.now
	p.tickPublish(fn.now)
	if len(p.searches) != 0 {
		t.Fatal("published before our own lookup")
	}
	p.pub.allowed = true
	p.tickPublish(fn.now)
	if len(p.searches) != 0 {
		t.Fatal("published while firewalled")
	}
	p.firewalledN = 2
	p.tickPublish(fn.now)
	if len(p.searches) != 2 {
		t.Fatalf("%d publishes started", len(p.searches))
	}
	// One of each kind per 2 seconds.
	p.tickPublish(fn.now.Add(time.Second))
	if len(p.searches) != 2 {
		t.Fatal("more than one publish per 2 seconds")
	}
}

// Keywords with more than 150 files rotate; nodes that report overload
// make the keyword wait.
func TestPublishRotationAndLoad(t *testing.T) {
	fn := newFakeNet()
	var shared []SharedFile
	for i := range 400 {
		shared = append(shared, SharedFile{Hash: [16]byte{byte(i), byte(i >> 8), 7}, Name: "Bright Comets part.avi", Size: 5, Complete: true})
	}
	p := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, Shared: func() (int, []SharedFile) { return 1, shared }})
	p.refreshShared()
	k := p.pub.byWord["bright"]
	if k == nil || len(k.files) != 400 {
		t.Fatal("keyword list")
	}
	o := fn.node(t, 2, Config{})
	p.table.Add(contactOf(o), false, false, t0)
	p.table.SetAlive(o.ID(), t0)
	p.pub.allowed, p.firewalledN, p.lastContact = true, 2, fn.now
	seen := map[[16]byte]bool{}
	for round := range 3 {
		now := fn.now.Add(time.Duration(round) * 25 * time.Hour)
		p.lastContact = now
		clear(p.searches)
		k.next = time.Time{}
		for i := range 10 {
			p.pub.kwNext = time.Time{}
			for id, s := range p.searches {
				if id != k.id {
					delete(p.searches, s.target) // the other words: out of the way
				}
			}
			p.tickPublish(now.Add(time.Duration(i) * 3 * time.Second))
			if s := p.searches[k.id]; s != nil {
				if len(s.files) != maxPublishPerKeyword {
					t.Fatalf("%d files in a publish", len(s.files))
				}
				for _, f := range s.files {
					seen[f] = true
				}
				break
			}
		}
	}
	if len(seen) < 400 {
		t.Fatalf("only %d files published in 3 rounds", len(seen))
	}
	// Overloaded nodes: the keyword waits.
	s := newSearch(searchStoreKeyword, k.id, fn.now)
	s.loadSum, s.loadN = 160, 2
	p.storeEnded(s, fn.now)
	if until := p.index.load[k.id]; until.Sub(fn.now) < 5*24*time.Hour {
		t.Fatalf("waits until %v", until)
	}
}

func TestNameWords(t *testing.T) {
	got := strings.Join(nameWords("Quiet.Orchard.2019.mkv"), ",")
	if got != "quiet,orchard,2019" {
		t.Fatalf("%q", got)
	}
	if got := strings.Join(nameWords("Blue Harbor.Version 1.2"), ","); !strings.Contains(got, "harbor") {
		t.Fatalf("%q", got)
	}
}

func TestIndexFile(t *testing.T) {
	x := newIndex()
	me := RandomID()
	e := kwEntry("Quiet Orchard.mkv", 100, pubIP(1, false), StrTag(TagFileType, "Video"))
	x.addKeyword(e, t0)
	old := kwEntry("Old.mkv", 100, pubIP(2, false))
	old.expires = t0.Add(-time.Hour)
	x.insertKeyword(old)
	x.addSource(&entry{key: RandomID(), source: RandomID(), ip: pubIP(3, false), tcpPort: 1, udpPort: 2, size: 9, tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(time.Hour)})
	x.load[RandomID()] = t0.Add(time.Hour)
	b := x.encode(me, t0)
	y := newIndex()
	if err := y.decode(b, me, t0); err != nil {
		t.Fatal(err)
	}
	if y.nKeyword != 1 || y.nSource != 1 || len(y.load) != 1 {
		t.Fatalf("read back %d keywords, %d sources, %d loads", y.nKeyword, y.nSource, len(y.load))
	}
	got := y.keywords[e.key][e.source][0]
	if got.name() != "Quiet Orchard.mkv" || TagStr(got.tags, TagFileType) != "Video" || len(got.publishers) != 1 || y.pubSubnets[subnet(pubIP(1, false))] != 1 {
		t.Fatalf("entry %+v", got)
	}
	// Another ID: not the node for those keywords any more.
	z := newIndex()
	z.decode(b, RandomID(), t0)
	if z.nKeyword != 0 || z.nSource != 1 {
		t.Fatal("keywords kept for another ID")
	}
	// Broken files fail without panicking.
	for i := range 2000 {
		c := append([]byte(nil), b...)
		rand.Read(c[20+i%(len(c)-20):][:1])
		newIndex().decode(c[:len(c)-i%len(c)], me, t0)
	}
}

// Random publishes and searches aimed at our part of the ID space never
// panic, and stay within the limits.
func TestStoreHostile(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	a.udp.verified = true
	me := a.ID()
	ops := []byte{OpPublishKeyReq, OpPublishSourceReq, OpPublishNotesReq, OpSearchKeyReq, OpSearchSourceReq, OpSearchNotesReq, OpPublishRes, OpPublishResV1}
	buf := make([]byte, 400)
	for i := range 20000 {
		rand.Read(buf)
		n := 16 + int(buf[0])%(len(buf)-16)
		w := me.Wire()
		copy(buf, w[:]) // aimed at us
		if i%3 == 0 {
			// Well-formed tag lists now and then.
			var t proto.Writer
			WriteID(&t, me)
			if i%2 == 0 {
				t.U16(1)
			}
			WriteID(&t, RandomID())
			WriteTags(&t, []proto.Tag{StrTag(TagFileName, "Quiet Orchard"), IntTag(TagFileSize, uint64(i)), IntTag(TagSourceType, 1), IntTag(TagSourcePort, 4662)})
			copy(buf, t.B)
			n = max(n, len(t.B))
		}
		from := netip.AddrPortFrom(pubIP(2000+i%500, false), uint16(1+i))
		d := rawPacket(ops[i%len(ops)], buf[:n])
		if i%2 == 1 { // proven: the sender has our key
			d = (&Keys{UDPKey: 1}).EncryptKad(d, addrOf(a).Addr(), ID{}, a.keys.VerifyKey(from.Addr()))
		}
		fn.queue = append(fn.queue, dgram{from, addrOf(a), d})
		fn.pump()
		fn.now = fn.now.Add(time.Second)
	}
	if a.index.nKeyword > maxKeywordEntries || a.index.nSource > maxSourceEntries || a.index.nNotes > maxNotesEntries {
		t.Fatal("limits passed")
	}
	if a.index.nKeyword+a.index.nSource+a.index.nNotes == 0 {
		t.Fatal("nothing stored: the test does not reach the store")
	}
}

// Publish answers are only taken from nodes we published to.
func TestPublishResTracked(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	s := newSearch(searchStoreFile, RandomID(), fn.now)
	a.searches[s.target] = s
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	var w proto.Writer
	WriteID(&w, s.target)
	w.U8(5)
	fn.inject(x, a, rawPacket(OpPublishRes, w.B))
	if s.answers != 0 {
		t.Fatal("unrequested publish answer counted")
	}
	a.outTracked = append(a.outTracked, tracked{ip: x.Addr(), op: OpPublishSourceReq, at: fn.now})
	fn.inject(x, a, rawPacket(OpPublishRes, w.B))
	if s.answers != 1 {
		t.Fatal("requested publish answer not counted")
	}
}

// Publishing starts once our own lookup has ended.
func TestPublishAfterOwnLookup(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	s := newSearch(searchNodeComplete, a.ID(), fn.now)
	a.searches[s.target] = s
	a.tickSearches(fn.now.Add(time.Minute))
	if !a.pub.allowed {
		t.Fatal("publishing not allowed after our own lookup")
	}
}
