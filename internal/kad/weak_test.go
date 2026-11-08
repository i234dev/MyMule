package kad

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"mymule/internal/proto"
)

// Publishes proven only by a ping vote for nothing: no trust, no name, no
// popularity.
func TestWeakPublishersDoNotVote(t *testing.T) {
	x := newIndex()
	first := kwEntry("Quiet Orchard.mkv", 10, pubIP(1, false))
	x.addKeyword(first, t0)
	for i := 2; i <= 60; i++ {
		e := kwEntry("Spam Name.mkv", 10, pubIP(i, false))
		e.source = first.source
		e.weak = true
		x.addKeyword(e, t0)
	}
	got := x.keywords[first.key][first.source][0]
	if got.name() != "Quiet Orchard.mkv" || len(got.names) != 1 {
		t.Fatalf("weak publishers renamed it: %q, %d names", got.name(), len(got.names))
	}
	if tr := x.trust(got, t0); tr > 10.01 {
		t.Fatalf("trust %v from weak publishers", tr)
	}
	if len(x.pubSubnets) != 1 {
		t.Fatalf("%d subnets counted", len(x.pubSubnets))
	}
	// A weak publisher that later publishes with our key counts.
	e := kwEntry("Quiet Orchard.mkv", 10, pubIP(2, false))
	e.source = first.source
	x.addKeyword(e, t0.Add(time.Minute))
	if len(x.pubSubnets) != 2 {
		t.Fatalf("%d subnets after a proven republish", len(x.pubSubnets))
	}
	// Cleaning drops weak publishers without touching the counts.
	x.clean(t0.Add(48 * time.Hour))
	if len(x.pubSubnets) != 0 {
		t.Fatalf("%d subnets left after cleaning", len(x.pubSubnets))
	}
}

// Pending publishes are stored weak once proved.
func TestPendingStoredWeak(t *testing.T) {
	file := RandomID()
	fn, a, b := storeNode(t, file)
	var w proto.Writer
	WriteID(&w, file)
	WriteID(&w, RandomID())
	WriteTags(&w, []proto.Tag{IntTag(TagSourceType, 1), IntTag(TagSourcePort, 4662)})
	fn.inject(addrOf(b), a, rawPacket(OpPublishSourceReq, w.B))
	if len(a.index.sources[file]) != 1 || !a.index.sources[file][0].weak {
		t.Fatal("not stored weak")
	}
	// The ping was not counted among our tracked requests.
	for _, tr := range a.outTracked {
		if tr.op == OpPing {
			t.Fatal("ping for a publish tracked")
		}
	}
}

// When our ping carried our key, a forged plain pong proves nothing; and a
// pong for a publish does not count as an external port answer.
func TestPendingNeedsKeyedPong(t *testing.T) {
	file := RandomID()
	fn, a, _ := storeNode(t, file)
	x := netip.AddrPortFrom(pubIP(80, false), 4672)
	e := &entry{key: file, ip: x.Addr(), tcpPort: 1, udpPort: 1, tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: fn.now.Add(time.Hour)}
	a.add(incoming{from: x, senderKey: UDPKey{Key: 99, IP: a.publicIP()}}, pendSource, e, fn.now)
	if !a.pending[x.Addr()].needKey {
		t.Fatal("ping with a key not marked")
	}
	var pong proto.Writer
	pong.U16(1234)
	fn.inject(x, a, rawPacket(OpPong, pong.B)) // forged: plain
	if len(a.index.sources[file]) != 0 {
		t.Fatal("stored on a plain pong")
	}
	if len(a.extPortIPs) != 0 {
		t.Fatal("publish pong taken as an external port answer")
	}
	a.outTracked = append(a.outTracked, tracked{ip: x.Addr(), op: OpPing, at: fn.now}) // also asked for our port
	keyed := (&Keys{Secret: [32]byte{5}}).EncryptKad(rawPacket(OpPong, pong.B), addrOf(a).Addr(), ID{}, a.keys.VerifyKey(x.Addr()))
	fn.inject(x, a, keyed)
	if len(a.index.sources[file]) != 1 {
		t.Fatal("not stored on a pong with our key")
	}
	if len(a.extPortIPs) != 0 {
		t.Fatal("publish pong taken as an external port answer")
	}
}

// Search tree strings are bounded in words and length.
func TestSearchTreeWordLimit(t *testing.T) {
	str := func(s string) []byte {
		var w proto.Writer
		w.U8(seString)
		w.Str16(s)
		return w.B
	}
	if _, err := parseTree(proto.NewReader(str(strings.Repeat("ab ", 60))), 0); err != nil {
		t.Fatalf("60 words refused: %v", err)
	}
	if _, err := parseTree(proto.NewReader(str(strings.Repeat("a ", 100))), 0); err == nil {
		t.Fatal("100 words accepted")
	}
	if _, err := parseTree(proto.NewReader(str(strings.Repeat("a", 300))), 0); err == nil {
		t.Fatal("300-byte string accepted")
	}
	// The words of all strings count together.
	two := append(append([]byte{seBool, 0}, str(strings.Repeat("ab ", 40))...), str(strings.Repeat("cd ", 40))...)
	if _, err := parseTree(proto.NewReader(two), 0); err == nil {
		t.Fatal("80 words in two strings accepted")
	}
}

// A full keyword index still takes refreshes of what it has.
func TestFullIndexTakesRefreshes(t *testing.T) {
	x := newIndex()
	e := kwEntry("Quiet Orchard.mkv", 10, pubIP(1, false))
	x.addKeyword(e, t0)
	x.nKeyword = maxKeywordEntries
	again := kwEntry("Quiet Orchard.mkv", 10, pubIP(1, false))
	again.source = e.source
	again.expires = t0.Add(30 * time.Hour)
	if ok, _ := x.addKeyword(again, t0.Add(6*time.Hour)); !ok {
		t.Fatal("refresh refused when full")
	}
	if ok, _ := x.addKeyword(kwEntry("Blue Harbor.mkv", 10, pubIP(2, false)), t0); ok {
		t.Fatal("new entry taken when full")
	}
}

// A weak publisher sharing a /24 with a proven one does not dilute or add
// to its trust.
func TestTrustIgnoresWeak(t *testing.T) {
	x := newIndex()
	e := kwEntry("Quiet Orchard.mkv", 10, pubIP(1, true))
	x.addKeyword(e, t0)
	w := kwEntry("Quiet Orchard.mkv", 10, pubIP(2, true))
	w.source, w.weak = e.source, true
	x.addKeyword(w, t0)
	got := x.keywords[e.key][e.source][0]
	got.trustAt = time.Time{}
	if tr := x.trust(got, t0); tr != 10 {
		t.Fatalf("trust %v", tr)
	}
}

// Names are cut as they are read, before waiting for a ping.
func TestPendingNamesCut(t *testing.T) {
	key := KeywordID("orchard")
	fn, a, _ := storeNode(t, key)
	x := netip.AddrPortFrom(pubIP(81, false), 4672)
	var w proto.Writer
	WriteID(&w, key)
	w.U16(1)
	WriteID(&w, RandomID())
	WriteTags(&w, []proto.Tag{StrTag(TagFileName, "orchard "+strings.Repeat("n", 3000)), IntTag(TagFileSize, 5)})
	fn.inject(x, a, rawPacket(OpPublishKeyReq, w.B))
	p := a.pending[x.Addr()]
	if p == nil || len(p.items) != 1 || len(p.items[0].e.names[0].name) > maxNameBytes {
		t.Fatal("long name kept while waiting")
	}
}
