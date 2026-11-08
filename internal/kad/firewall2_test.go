package kad

import (
	"net/netip"
	"testing"
	"time"

	"mymule/internal/proto"
)

// Clients that took the request and never answered count against: two of
// them mean firewalled. One that could not be asked does not count.
func TestUDPTestNoAnswers(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{CheckUDP: func(netip.Addr, uint16, uint16, uint16, uint32) bool { return true }})
	knowsExtPort(a, 4672)
	a.lastContact = fn.now
	a.startUDPTest(fn.now)
	knowsExtPort(a, 4672)
	failed := contactN(40)
	a.udp.candidates = []Contact{failed}
	a.tickUDPTest(fn.now)
	a.FailedUDPTest(failed.IP)
	for i := range 2 {
		a.udp.candidates = []Contact{contactN(41 + i)}
		a.tickUDPTest(fn.now)
	}
	if a.udp.verified {
		t.Fatal("result without answers")
	}
	fn.now = fn.now.Add(udpTestAnswerWait + time.Second)
	a.lastContact = fn.now
	a.tickUDPTest(fn.now)
	if !a.udp.verified || !a.udp.firewalled || a.udp.running {
		t.Fatalf("not firewalled after two silent clients: %+v", a.udp)
	}
	// Candidates whose TCP port cannot be connected to are skipped.
	a.startUDPTest(fn.now)
	knowsExtPort(a, 4672)
	bad := contactN(50)
	bad.TCPPort = 0
	a.udp.candidates = []Contact{bad}
	a.tickUDPTest(fn.now)
	if len(a.udp.asked) != 0 {
		t.Fatal("candidate without a TCP port asked")
	}
}

// A retest that gets no result keeps the last one while our TCP port is
// open (as eMule).
func TestUDPRetestTimeout(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{CheckUDP: func(netip.Addr, uint16, uint16, uint16, uint32) bool { return true }})
	a.firewalledN = 2 // TCP open
	a.udp.verified, a.udp.firewalled = true, false
	a.lastContact = fn.now
	a.startUDPTest(fn.now)
	fn.now = fn.now.Add(udpTestTimeout + time.Second)
	a.lastContact = fn.now
	knowsExtPort(a, 4672)
	a.tickUDPTest(fn.now)
	if a.udp.running || a.udp.firewalled || !a.udpReachable() {
		t.Fatalf("open TCP node made UDP-firewalled by a timeout: %+v", a.udp)
	}
	// Never tested before, and firewalled on TCP too: then the timeout
	// says firewalled.
	a.firewalledN = 0
	a.recheckIP = firewallChecks
	a.udp = udpTest{}
	a.startUDPTest(fn.now)
	fn.now = fn.now.Add(udpTestTimeout + time.Second)
	a.lastContact = fn.now
	knowsExtPort(a, 4672)
	a.tickUDPTest(fn.now)
	if !a.udp.firewalled {
		t.Fatal("not firewalled after a timeout with TCP firewalled")
	}
}

// What we pass to the node we serve is limited, whatever the senders.
func TestRelayLimits(t *testing.T) {
	fn, a, b, _ := buddyPair(t)
	relayed := 0
	b.served = &buddyLink{ip: addrOf(a).Addr(), userHash: a.keys.UserHash, id: a.buddyID(), connected: true,
		send: func(op byte, p []byte) bool { relayed++; return true }}
	id := a.buddyID()
	for i := range maxRelaysPerMin + 20 {
		x := netip.AddrPortFrom(pubIP(500+i, false), 4672)
		var w proto.Writer
		WriteID(&w, id)
		WriteID(&w, RandomID())
		w.U16(5555)
		fn.inject(x, b, rawPacket(OpCallbackReq, w.B))
	}
	b.runDeferred()
	if relayed != maxRelaysPerMin {
		t.Fatalf("%d relayed in a minute", relayed)
	}
	// A huge reask is not relayed.
	fn.now = fn.now.Add(2 * time.Minute)
	relayed = 0
	big := append(append([]byte{ProtoEMule, OpReaskCallbackUDP}, id[:]...), make([]byte, maxReaskBytes)...)
	fn.inject(netip.AddrPortFrom(pubIP(900, false), 4672), b, big)
	b.runDeferred()
	if relayed != 0 {
		t.Fatal("huge reask relayed")
	}
}

// Type 6 sources carry the direct-callback connect option eMule requires.
func TestType6ConnectOption(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }})
	a.recheckIP = firewallChecks
	a.udp.verified = true
	tags := wire(a.sourceTags(SharedFile{Size: 1}))
	if o, _ := TagInt(tags, TagEncryption); o&ConnectDirectCallback == 0 {
		t.Fatalf("connect options %x", o)
	}
	// Open: no such option.
	a.firewalledN = 2
	if o, _ := TagInt(wire(a.sourceTags(SharedFile{Size: 1})), TagEncryption); o&ConnectDirectCallback != 0 {
		t.Fatalf("connect options %x when open", o)
	}
}

// A recheck that cannot ask anyone keeps a result that worked.
func TestUDPRecheckKeepsVerified(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{CheckUDP: func(netip.Addr, uint16, uint16, uint16, uint32) bool { return true }})
	a.recheckIP = firewallChecks // TCP firewalled
	a.udp.verified, a.udp.firewalled = true, false
	a.lastContact = fn.now
	a.startUDPTest(fn.now)
	fn.now = fn.now.Add(udpTestTimeout + time.Second)
	a.lastContact = fn.now
	knowsExtPort(a, 4672)
	a.tickUDPTest(fn.now)
	if a.udp.firewalled || !a.udpReachable() {
		t.Fatal("a recheck without answers undid an open result")
	}
}

// Invalid direct callback requests take nothing from valid ones.
func TestDirectCallbackInvalid(t *testing.T) {
	fn := newFakeNet()
	n := 0
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, ConnectTo: func(netip.Addr, uint16) { n++ }})
	a.recheckIP = firewallChecks
	a.udp.verified = true
	x := netip.AddrPortFrom(pubIP(1000, false), 4672)
	bad := []byte{ProtoEMule, OpDirectCallback, 0, 0}
	bad = append(bad, make([]byte, 17)...)
	fn.inject(x, a, bad) // TCP port 0
	good := []byte{ProtoEMule, OpDirectCallback, 0xB3, 0x15}
	good = append(good, make([]byte, 17)...)
	fn.now = fn.now.Add(10 * time.Second)
	fn.inject(x, a, good)
	a.runDeferred()
	if n != 1 {
		t.Fatalf("%d callbacks after an invalid then a valid request", n)
	}
}

// Lookups for buddies (callbacks to sources without their buddy's
// address) run a few at a time.
func TestCallbackLookupsLimited(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }})
	for i := 10; i < 30; i++ {
		a.table.Add(contactN(i), false, false, t0)
	}
	a.lastContact = fn.now
	started := 0
	for i := range maxCallbackLookups + 5 {
		if a.RequestCallback(netip.Addr{}, 0, ID{byte(i + 1)}, [16]byte{1}) == nil {
			started++
		}
	}
	if started != maxCallbackLookups {
		t.Fatalf("%d lookups started", started)
	}
}
