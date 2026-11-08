package kad

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"mymule/internal/proto"
)

// knowsExtPort makes a test node skip finding its external port.
func knowsExtPort(n *Node, port uint16) {
	n.extPort = port
	n.extPortIPs = []netip.Addr{pubIP(1, false), pubIP(2, false), pubIP(3, false)}
}

func fwUDP(code byte, port uint16) []byte {
	var w proto.Writer
	w.U8(code)
	w.U16(port)
	return rawPacket(OpFirewallUDP, w.B)
}

// The UDP test: a client we never talked to sends us the test packet, with
// the key we gave it; one is enough to know we are reachable.
func TestUDPTestOpen(t *testing.T) {
	fn := newFakeNet()
	type ask struct {
		ip                  netip.Addr
		tcp, intern, extern uint16
		key                 uint32
	}
	var asks []ask
	var b *Node
	a := fn.node(t, 1, Config{CheckUDP: func(ip netip.Addr, tcp, intern, extern uint16, key uint32) bool {
		asks = append(asks, ask{ip, tcp, intern, extern, key})
		return true
	}})
	b = fn.node(t, 2, Config{})
	knowsExtPort(a, 4672)
	a.lastContact = fn.now
	a.startUDPTest(fn.now)
	knowsExtPort(a, 4672)
	a.udp.candidates = []Contact{contactOf(b)}
	a.tickUDPTest(fn.now)
	a.runDeferred()
	if len(asks) != 1 || asks[0].ip != addrOf(b).Addr() || asks[0].tcp != 4662 || asks[0].intern != 4672 || asks[0].key != a.keys.VerifyKey(addrOf(b).Addr()) {
		t.Fatalf("asks %+v", asks)
	}
	// A plain answer (forgeable) or one from elsewhere does nothing.
	fn.inject(addrOf(b), a, fwUDP(0, 4672))
	x := netip.AddrPortFrom(pubIP(66, false), 4672)
	fn.inject(x, a, (&Keys{Secret: [32]byte{3}}).EncryptKad(fwUDP(0, 4672), addrOf(a).Addr(), ID{}, a.keys.VerifyKey(x.Addr())))
	if a.udp.verified {
		t.Fatal("verified by an answer that proves nothing")
	}
	// B runs the test as a real client would.
	var w proto.Writer
	w.U16(asks[0].intern)
	w.U16(asks[0].extern)
	w.U32(asks[0].key)
	b.TestUDPFor(addrOf(a).Addr(), w.B)
	fn.pump()
	if !a.udp.verified || a.udpFirewalled() || a.udp.running || !a.udpReachable() || !a.storing() {
		t.Fatalf("after the answer: %+v", a.udp)
	}
}

// Two clients that got through with an error, or none in six minutes:
// firewalled.
func TestUDPTestFirewalled(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{CheckUDP: func(netip.Addr, uint16, uint16, uint16, uint32) bool { return true }})
	knowsExtPort(a, 4672)
	a.lastContact = fn.now
	a.startUDPTest(fn.now)
	knowsExtPort(a, 4672)
	// An answer naming another port, or an error, does not count as open.
	for i, code := range []byte{1, 0} {
		c := contactN(30 + i)
		a.udp.candidates = []Contact{c}
		a.tickUDPTest(fn.now)
		port := uint16(4672)
		if code == 0 {
			port = 9
		}
		fn.inject(c.Addr(), a, (&Keys{Secret: [32]byte{3}}).EncryptKad(fwUDP(code, port), addrOf(a).Addr(), ID{}, a.keys.VerifyKey(c.IP)))
	}
	if a.udp.verified {
		t.Fatal("open without a good answer")
	}
	fn.now = fn.now.Add(udpTestTimeout + time.Second)
	a.lastContact = fn.now
	a.tickUDPTest(fn.now)
	if !a.udp.verified || !a.udpFirewalled() || a.storing() {
		t.Fatalf("not firewalled after the timeout: %+v", a.udp)
	}
	// The hello says so.
	b := fn.node(t, 2, Config{})
	a.mu.Lock()
	a.sendHello(OpHelloReq, addrOf(b), Version, 0, b.ID(), false)
	a.mu.Unlock()
	fn.pump()
	if b.table.Get(a.ID()) != nil {
		t.Fatal("UDP-firewalled node added to a routing table")
	}
}

// Candidates come from a lookup, and stay out of the routing table.
func TestUDPTestCandidates(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{CheckUDP: func(netip.Addr, uint16, uint16, uint16, uint32) bool { return false }})
	b := fn.node(t, 2, Config{})
	for i := 10; i < 30; i++ {
		c := contactN(i)
		b.table.Add(c, false, false, t0)
		b.table.SetAlive(c.ID, t0)
	}
	a.table.Add(contactOf(b), false, false, t0)
	a.table.SetAlive(b.ID(), t0)
	knowsExtPort(a, 4672)
	a.lastContact = fn.now
	a.startUDPTest(fn.now)
	knowsExtPort(a, 4672)
	a.tickUDPTest(fn.now)
	fn.pump()
	if len(a.udp.candidates) == 0 {
		t.Fatal("no candidates from the lookup")
	}
	for _, c := range a.udp.candidates {
		if a.table.Get(c.ID) != nil {
			t.Fatal("candidate added to the table")
		}
	}
}

// buddyPair sets up A firewalled on TCP and UDP, and B open, near A's
// buddy ID, and A's search for a buddy.
func buddyPair(t *testing.T) (*fakeNet, *Node, *Node, *[]netip.Addr) {
	fn := newFakeNet()
	var connects []netip.Addr
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, ConnectBuddy: func(ip netip.Addr, tcp uint16, user [16]byte) bool {
		connects = append(connects, ip)
		return true
	}})
	b := fn.node(t, 2, Config{TCPPort: func() uint16 { return 4662 }})
	a.recheckIP = firewallChecks // checked: firewalled
	a.udp.verified, a.udp.firewalled = true, true
	b.firewalledN = 2
	b.udp.verified = true
	near := a.buddyID()
	near[15] ^= 1
	setID(b, near)
	a.table.Add(contactOf(b), false, false, t0)
	a.table.SetAlive(b.ID(), t0)
	a.lastContact = fn.now
	return fn, a, b, &connects
}

// The whole buddy cycle: A finds B, connects, and a downloader's callback
// request reaches A through B.
func TestBuddyCallback(t *testing.T) {
	fn, a, b, connects := buddyPair(t)
	a.tickBuddy(fn.now)
	fn.pump()
	for range 5 {
		fn.now = fn.now.Add(time.Second)
		a.lastContact = fn.now
		a.mu.Lock()
		a.tickSearches(fn.now)
		a.mu.Unlock()
		fn.pump()
	}
	a.runDeferred()
	if len(*connects) != 1 || (*connects)[0] != addrOf(b).Addr() {
		t.Fatalf("connects %v", *connects)
	}
	if o := b.buddyOffers[addrOf(a).Addr()]; o == nil || o.id != a.buddyID() {
		t.Fatal("B did not agree")
	}
	if !b.IsBuddyOffer(addrOf(a).Addr(), a.keys.UserHash) || b.IsBuddyOffer(addrOf(a).Addr(), [16]byte{1}) {
		t.Fatal("IsBuddyOffer")
	}
	// The TCP connection: B recognises A; A knows its buddy.
	var relayed [][]byte
	if b.IncomingBuddy(netip.MustParseAddr("9.9.9.9"), a.keys.UserHash, nil) {
		t.Fatal("another address accepted as the buddy")
	}
	if !b.IncomingBuddy(addrOf(a).Addr(), a.keys.UserHash, func(op byte, p []byte) bool {
		if op == OpCallback {
			relayed = append(relayed, p)
		}
		return true
	}) {
		t.Fatal("B did not take A's connection")
	}
	a.BuddyUp(addrOf(b).Addr(), addrOf(b).Port())
	if fw := a.Firewall(); fw.BuddyIP != addrOf(b).Addr() || fw.BuddyPort != addrOf(b).Port() {
		t.Fatalf("firewall state %+v", fw)
	}
	// A publishes its sources with B's address.
	tags := wire(a.sourceTags(SharedFile{Size: 10}))
	if typ, _ := TagInt(tags, TagSourceType); typ != 3 {
		t.Fatalf("source type %d", typ)
	}
	src := Source{}
	n := a // read them back as a downloader would
	s := newSearch(searchFile, RandomID(), fn.now)
	s.onSource = func(x Source) { src = x }
	n.fileResult(s, RandomID(), append(tags, wire([]proto.Tag{IntTag(TagSourceIP, uint64(ipToU32(addrOf(a).Addr())))})...))
	n.runDeferred()
	if src.BuddyIP != addrOf(b).Addr() || src.BuddyPort != addrOf(b).Port() || src.Buddy != a.buddyID() || src.Direct() {
		t.Fatalf("source as read: %+v", src)
	}
	// A downloader asks B for a callback.
	c := fn.node(t, 3, Config{TCPPort: func() uint16 { return 5555 }})
	c.lastContact = fn.now
	file := [16]byte{7}
	if err := c.RequestCallback(src.BuddyIP, src.BuddyPort, src.Buddy, file); err != nil {
		t.Fatal(err)
	}
	fn.pump()
	b.runDeferred()
	if len(relayed) != 1 {
		t.Fatalf("%d callbacks relayed", len(relayed))
	}
	ip, port, f, ok := a.Callback(relayed[0])
	if !ok || ip != addrOf(c).Addr() || port != 5555 || f != file {
		t.Fatalf("callback %v %v:%d %x", ok, ip, port, f)
	}
	// Not meant for us: refused.
	bad := append([]byte(nil), relayed[0]...)
	bad[0] ^= 1
	if _, _, _, ok := a.Callback(bad); ok {
		t.Fatal("callback for another node taken")
	}
	// A request with another buddy ID is not relayed.
	c.RequestCallback(src.BuddyIP, src.BuddyPort, RandomID(), file)
	fn.now = fn.now.Add(2 * time.Minute) // past the flood limit
	fn.pump()
	b.runDeferred()
	if len(relayed) != 1 {
		t.Fatal("callback for another buddy relayed")
	}
	// A loses its buddy: it looks again, and stops publishing type 3.
	a.BuddyDown(addrOf(b).Addr())
	if a.mine != nil || a.sourceTags(SharedFile{Size: 1}) != nil {
		t.Fatal("still a buddy after it went")
	}
}

// An open node serves one firewalled node at a time, and none while it is
// firewalled itself.
func TestBuddyServeConditions(t *testing.T) {
	fn, a, b, _ := buddyPair(t)
	req := func(from *Node) {
		var w proto.Writer
		WriteID(&w, from.buddyID())
		WriteID(&w, ID(from.keys.UserHash))
		w.U16(4662)
		from.mu.Lock()
		from.send(addrOf(b), OpFindBuddyReq, w.B, b.ID(), 0)
		from.mu.Unlock()
		fn.pump()
	}
	req(a)
	other := fn.node(t, 4, Config{})
	req(other)
	if len(b.buddyOffers) != 2 {
		t.Fatalf("%d offers: a forged request must not keep others out", len(b.buddyOffers))
	}
	// The first that connects is served; the others are dropped.
	if !b.IncomingBuddy(addrOf(other).Addr(), other.keys.UserHash, func(byte, []byte) bool { return true }) {
		t.Fatal("not served")
	}
	if b.IncomingBuddy(addrOf(a).Addr(), a.keys.UserHash, func(byte, []byte) bool { return true }) {
		t.Fatal("two served")
	}
	b.ServedBuddyGone(addrOf(other).Addr())
	// Offers not taken expire.
	fn.now = fn.now.Add(time.Minute)
	req(a)
	fn.now = fn.now.Add(buddyWait + time.Second)
	b.tickBuddy(fn.now)
	if len(b.buddyOffers) != 0 {
		t.Fatal("offer kept")
	}
	b.firewalledN = 0
	b.recheckIP = firewallChecks
	fn.now = fn.now.Add(time.Minute) // past the flood limit
	req(a)
	if len(b.buddyOffers) != 0 {
		t.Fatal("a firewalled node offered to be a buddy")
	}
}

// A node firewalled on TCP but reachable over UDP takes direct callbacks.
func TestDirectCallback(t *testing.T) {
	fn := newFakeNet()
	var connects []netip.AddrPort
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, ConnectTo: func(ip netip.Addr, tcp uint16) {
		connects = append(connects, netip.AddrPortFrom(ip, tcp))
	}})
	a.recheckIP = firewallChecks
	a.udp.verified = true
	if typ, _ := TagInt(wire(a.sourceTags(SharedFile{Size: 1})), TagSourceType); typ != 6 {
		t.Fatalf("source type %d", typ)
	}
	if !a.Firewall().DirectCallback {
		t.Fatal("direct callback not announced")
	}
	c := fn.node(t, 2, Config{TCPPort: func() uint16 { return 5555 }})
	if c.DirectCallback(addrOf(a).Addr(), addrOf(a).Port()) == nil {
		t.Fatal("direct callback asked while firewalled ourselves")
	}
	c.firewalledN = 2 // open: a can reach us
	c.DirectCallback(addrOf(a).Addr(), addrOf(a).Port())
	fn.pump()
	a.runDeferred()
	if len(connects) != 1 || connects[0] != netip.AddrPortFrom(addrOf(c).Addr(), 5555) {
		t.Fatalf("connects %v", connects)
	}
	// Once per address per 3 minutes.
	c.DirectCallback(addrOf(a).Addr(), addrOf(a).Port())
	fn.pump()
	a.runDeferred()
	if len(connects) != 1 {
		t.Fatal("second callback within 3 minutes")
	}
	// Not when open on TCP (no need) or not reachable on UDP.
	a.firewalledN = 2
	fn.now = fn.now.Add(4 * time.Minute)
	c.DirectCallback(addrOf(a).Addr(), addrOf(a).Port())
	fn.pump()
	a.runDeferred()
	if len(connects) != 1 {
		t.Fatal("callback taken while open")
	}
	// The addresses remembered are limited: full, one is forgotten.
	a.firewalledN = 0
	clear(a.directAsked)
	for i := range maxDirectAsked {
		a.directAsked[pubIP(500000+i, false)] = fn.now
	}
	fn.now = fn.now.Add(time.Second)
	c.firewalledN = 2
	pkt := []byte{ProtoEMule, OpDirectCallback, 0xB3, 0x15}
	pkt = append(pkt, make([]byte, 17)...)
	fn.inject(netip.AddrPortFrom(pubIP(77, false), 4672), a, pkt)
	a.runDeferred()
	if len(connects) != 2 || len(a.directAsked) != maxDirectAsked {
		t.Fatalf("while full: %d remembered, connects %v", len(a.directAsked), connects)
	}
}

// As a buddy, a UDP reask for our node goes to it over TCP, with the
// asker's address.
func TestReaskRelay(t *testing.T) {
	fn, a, b, _ := buddyPair(t)
	var relayed []byte
	b.served = &buddyLink{ip: addrOf(a).Addr(), userHash: a.keys.UserHash, id: a.buddyID(), connected: true,
		send: func(op byte, p []byte) bool {
			if op == OpReaskCallbackTCP {
				relayed = p
			}
			return true
		}}
	x := netip.AddrPortFrom(pubIP(70, false), 4672)
	id := a.buddyID()
	pkt := append([]byte{ProtoEMule, OpReaskCallbackUDP}, id[:]...)
	pkt = append(pkt, 1, 2, 3, 4)
	fn.inject(x, b, pkt)
	b.runDeferred()
	if len(relayed) != 10 {
		t.Fatalf("relayed % X", relayed)
	}
	a4 := x.Addr().As4()
	if [4]byte(relayed[:4]) != a4 || binary.LittleEndian.Uint16(relayed[4:6]) != x.Port() {
		t.Fatalf("address not put in: % X", relayed)
	}
	// For another node: not relayed.
	relayed = nil
	other := RandomID()
	fn.inject(x, b, append(append([]byte{ProtoEMule, OpReaskCallbackUDP}, other[:]...), 1, 2))
	b.runDeferred()
	if relayed != nil {
		t.Fatal("reask for another node relayed")
	}
}

// Without the buddy's address, a callback request goes to the nodes near
// the buddy ID.
func TestCallbackThroughSearch(t *testing.T) {
	fn, a, b, _ := buddyPair(t)
	_ = a
	c := fn.node(t, 3, Config{TCPPort: func() uint16 { return 5555 }})
	c.table.Add(contactOf(b), false, false, t0)
	c.table.SetAlive(b.ID(), t0)
	c.lastContact = fn.now
	if err := c.RequestCallback(netip.Addr{}, 0, b.ID(), [16]byte{1}); err != nil {
		t.Fatal(err)
	}
	if s := c.searches[b.ID()]; s == nil || s.typ != searchFindSource {
		t.Fatal("no search")
	}
}

// wire returns tags as they come out of a packet.
func wire(tags []proto.Tag) []proto.Tag {
	var w proto.Writer
	WriteTags(&w, tags)
	out, _ := ReadTags(proto.NewReader(w.B))
	return out
}

// The test packet goes to both ports when they differ.
func TestUDPTestTwoPorts(t *testing.T) {
	fn := newFakeNet()
	b := fn.node(t, 2, Config{})
	var w proto.Writer
	w.U16(4672)
	w.U16(5000)
	w.U32(7)
	x := pubIP(80, false)
	b.TestUDPFor(x, w.B)
	fn.pump()
	if len(fn.sent[netip.AddrPortFrom(x, 4672)]) != 1 || len(fn.sent[netip.AddrPortFrom(x, 5000)]) != 1 {
		t.Fatal("not one packet to each port")
	}
}
