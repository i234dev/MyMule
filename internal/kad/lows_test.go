package kad

import (
	"net/netip"
	"testing"
	"time"

	"mymule/internal/proto"
)

// Sources published without our key are given after the proven ones.
func TestWeakSourcesLast(t *testing.T) {
	file := RandomID()
	fn, a, _ := storeNode(t, file)
	src := func(i int, weak bool) *entry {
		return &entry{key: file, source: ID{byte(i)}, ip: pubIP(i, false), tcpPort: 4662, udpPort: 4672,
			tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: fn.now.Add(time.Hour), weak: weak}
	}
	a.index.addSource(src(1, false))
	a.index.addSource(src(2, true)) // newest first in the list
	var w proto.Writer
	WriteID(&w, file)
	w.U16(0)
	w.U64(0)
	x := netip.AddrPortFrom(pubIP(90, false), 4672)
	fn.inject(x, a, rawPacket(OpSearchSourceReq, w.B))
	var first ID
	for _, d := range fn.sent[x] {
		if p, err := DecodePacket(d); err == nil && p.Opcode == OpSearchRes {
			r := proto.NewReader(p.Payload)
			ReadID(r)
			ReadID(r)
			if r.U16() != 2 {
				t.Fatal("not both sources")
			}
			first = ReadID(r)
		}
	}
	if first != (ID{1}) {
		t.Fatalf("first source %s", first)
	}
}

// Publishes from addresses others could not reach (private, loopback...)
// are not stored, even with our key.
func TestPublishFromPrivateAddress(t *testing.T) {
	file := RandomID()
	fn, a, _ := storeNode(t, file)
	from := netip.MustParseAddrPort("10.0.0.5:4672")
	var w proto.Writer
	WriteID(&w, file)
	WriteID(&w, RandomID())
	WriteTags(&w, []proto.Tag{IntTag(TagSourceType, 1), IntTag(TagSourcePort, 4662)})
	d := (&Keys{Secret: [32]byte{3}}).EncryptKad(rawPacket(OpPublishSourceReq, w.B), addrOf(a).Addr(), ID{}, a.keys.VerifyKey(from.Addr()))
	fn.inject(from, a, d)
	if len(a.index.sources[file]) != 0 || len(a.pending) != 0 {
		t.Fatal("publish from a private address taken")
	}
	// The same from a public address is.
	pub := netip.AddrPortFrom(pubIP(91, false), 4672)
	d = (&Keys{Secret: [32]byte{3}}).EncryptKad(rawPacket(OpPublishSourceReq, w.B), addrOf(a).Addr(), ID{}, a.keys.VerifyKey(pub.Addr()))
	fn.inject(pub, a, d)
	if len(a.index.sources[file]) != 1 {
		t.Fatal("publish from a public address refused")
	}
}

// kadindex.dat keeps whether an entry is weak; sources from a file of
// the first version (which did not say) are taken as weak.
func TestIndexFileWeakFlag(t *testing.T) {
	x := newIndex()
	me := RandomID()
	for i, weak := range []bool{false, true} {
		x.addSource(&entry{key: ID{byte(i + 1)}, source: RandomID(), ip: pubIP(i+3, false), tcpPort: 1, udpPort: 2,
			tags: []proto.Tag{IntTag(TagSourceType, 1)}, expires: t0.Add(time.Hour), weak: weak})
	}
	b := x.encode(me, t0)
	y := newIndex()
	if err := y.decode(b, me, t0); err != nil {
		t.Fatal(err)
	}
	if y.sources[ID{1}][0].weak || !y.sources[ID{2}][0].weak {
		t.Fatal("weak flag lost")
	}
	// The same file as version 1 wrote it: no flag after each entry.
	var w proto.Writer
	w.U32(1)
	WriteID(&w, me)
	w.U32(0)
	w.U32(1)
	e := x.sources[ID{1}][0]
	WriteID(&w, e.key)
	WriteID(&w, e.source)
	a := e.ip.As4()
	w.Bytes(a[:])
	w.U16(e.udpPort)
	w.U16(e.tcpPort)
	w.U64(e.size)
	w.U64(uint64(e.expires.Unix()))
	WriteTags(&w, e.tags)
	w.U32(0)
	z := newIndex()
	if err := z.decode(w.B, me, t0); err != nil {
		t.Fatal(err)
	}
	if len(z.sources[ID{1}]) != 1 || !z.sources[ID{1}][0].weak {
		t.Fatal("version 1 source not taken as weak")
	}
}

// A KADEMLIA2_REQ asking for 31 contacts gets 11 at most.
func TestReqContactsCapped(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	for i := 10; i < 60; i++ {
		a.table.Add(contactN(i), false, false, t0)
	}
	a.table.walk(func(c *Contact) bool { c.typ = 0; return true }) // known for long
	var w proto.Writer
	w.U8(0x1F)
	WriteID(&w, RandomID())
	WriteID(&w, a.ID())
	x := netip.AddrPortFrom(pubIP(92, false), 4672)
	fn.inject(x, a, rawPacket(OpReq, w.B))
	got := -1
	for _, d := range fn.sent[x] {
		if p, err := DecodePacket(d); err == nil && p.Opcode == OpRes {
			got = int(p.Payload[IDLen])
		}
	}
	if got != findNode {
		t.Fatalf("%d contacts", got)
	}
}

// Only answers to our requests, or packets with our key, count as being
// connected: junk does not.
func TestLastContactOnlyFromAnswers(t *testing.T) {
	fn := newFakeNet()
	a, b := fn.node(t, 1, Config{}), fn.node(t, 2, Config{})
	x := netip.AddrPortFrom(pubIP(93, false), 4672)
	fn.inject(x, a, rawPacket(OpPing, nil))
	fn.inject(x, a, rawPacket(OpPong, []byte{1, 2}))
	fn.inject(x, a, rawPacket(0x77, []byte{1, 2, 3}))
	if !a.lastContact.IsZero() {
		t.Fatal("connected by requests and junk")
	}
	a.mu.Lock()
	a.send(addrOf(b), OpPing, nil, b.ID(), 0)
	a.mu.Unlock()
	fn.pump()
	if a.lastContact.IsZero() {
		t.Fatal("an answer did not count")
	}
}

// Buddy requests without our key take half the waiting places at most; one
// with it takes the place of the oldest of those.
func TestBuddyOffersUnproven(t *testing.T) {
	fn, _, b, _ := buddyPair(t)
	req := func(from netip.AddrPort, key uint32) {
		var w proto.Writer
		WriteID(&w, RandomID())
		WriteID(&w, RandomID())
		w.U16(4662)
		d := rawPacket(OpFindBuddyReq, w.B)
		if key != 0 {
			d = (&Keys{Secret: [32]byte{9}}).EncryptKad(d, addrOf(b).Addr(), ID{}, key)
		}
		fn.inject(from, b, d)
	}
	for i := range maxBuddyOffers {
		req(netip.AddrPortFrom(pubIP(200+i, false), 4672), 0)
		fn.now = fn.now.Add(time.Second)
	}
	if len(b.buddyOffers) != maxBuddyOffers/2 {
		t.Fatalf("%d unproven offers", len(b.buddyOffers))
	}
	for i := range maxBuddyOffers {
		from := netip.AddrPortFrom(pubIP(300+i, false), 4672)
		req(from, b.keys.VerifyKey(from.Addr()))
	}
	if len(b.buddyOffers) != maxBuddyOffers {
		t.Fatalf("%d offers", len(b.buddyOffers))
	}
	for _, o := range b.buddyOffers {
		if !o.proven {
			t.Fatal("unproven offer kept over a proven request")
		}
	}
}
