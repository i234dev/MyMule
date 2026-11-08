package kad

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"mymule/internal/proto"
)

// Firewall support (eMule's CUDPFirewallTester and the Kad buddy system).
//
// UDP test: whether other nodes can reach our Kad port without our having
// sent them anything first. We ask a couple of clients we never contacted
// over UDP (over TCP, OP_FWCHECKUDPREQ) to send us KADEMLIA2_FIREWALLUDP;
// one arriving means open, two failures mean firewalled. Only a node that
// is reachable stores what others publish.
//
// Buddies: a node behind a firewall (TCP and UDP) keeps a TCP connection
// to an open node near the opposite of its ID, its buddy, and publishes its
// sources with the buddy's address. A downloader sends the buddy a
// KADEMLIA_CALLBACK_REQ; the buddy passes it over TCP (OP_CALLBACK), and
// the firewalled node connects to the downloader. An open node serves as
// the buddy of one firewalled node.
//
// Direct callbacks: a node whose TCP port is firewalled but whose UDP is
// open publishes its sources with type 6; downloaders send it
// OP_DIRECTCALLBACKREQ over UDP and it connects to them.

// eD2K client UDP opcodes handled on the Kad port.
const (
	OpReaskCallbackUDP = 0x94 // <buddy ID 16>... (relayed by a buddy)
	OpDirectCallback   = 0x95 // <u16 tcp><user hash 16><u8 connect options>
)

// eD2K TCP opcodes of the buddy system.
const (
	OpCallback          = 0x99 // <ID check><ID file><u32 ip><u16 tcp>
	OpReaskCallbackTCP  = 0x9A
	OpBuddyPing         = 0x9F
	OpBuddyPong         = 0xA0
	OpFWCheckUDPReq     = 0xA7 // <u16 intern port><u16 extern port><u32 key>
	udpTestClients      = 2    // results needed (eMule's UDP_FIREWALLTEST_CLIENTSTOASK)
	udpTestTimeout      = 6 * time.Minute
	udpTestAnswerWait   = 2 * time.Minute
	findBuddyEvery      = 20 * time.Minute
	maxBuddyOffers      = 8               // firewalled nodes we wait for at once
	maxRelaysPerMin     = 30              // callbacks and reasks passed to our node
	maxReaskBytes       = 512             // a reask relayed
	buddyWait           = 2 * time.Minute // for the firewalled node to connect
	directCallbackEvery = 3 * time.Minute // per address
)

// udpTest is the state of the UDP firewall test.
type udpTest struct {
	running    bool
	start      time.Time
	firewalled bool // the result (or the last one while testing)
	verified   bool // a test has given a result
	lastFW     bool
	finished   int // tests that gave a result
	candidates []Contact
	asked      map[netip.Addr]*udpAsk
	useIntern  bool // nodes reached us on our local port, not the NAT's
	searchedAt time.Time
}

type udpAsk struct {
	at       time.Time
	answered bool
}

// buddyLink is our buddy, or the node we are the buddy of.
type buddyLink struct {
	ip        netip.Addr
	kadPort   uint16
	tcpPort   uint16
	userHash  [16]byte
	id        ID // the ID it looked for us with (served side)
	connected bool
	proven    bool      // its request had our key (served side, pending)
	until     time.Time // to connect (served side, pending)
	send      func(op byte, payload []byte) bool
}

// startUDPTest (re)starts the UDP firewall test, keeping the last result
// meanwhile.
func (n *Node) startUDPTest(now time.Time) {
	t := &n.udp
	t.lastFW = t.firewalled
	t.running = true
	t.start = now
	t.finished = 0
	t.candidates = nil
	t.asked = map[netip.Addr]*udpAsk{}
	t.searchedAt = time.Time{}
	n.extPortIPs, n.extPorts = nil, nil // find our external port again
}

// UDPFirewalled reports whether other nodes cannot reach our Kad port
// unasked (while a test runs, the last result).
func (n *Node) udpFirewalled() bool {
	if n.udp.running {
		return n.udp.lastFW
	}
	return n.udp.firewalled
}

// udpReachable reports whether a test showed that other nodes reach us.
func (n *Node) udpReachable() bool { return n.udp.verified && !n.udpFirewalled() }

func (n *Node) localPort() uint16 {
	if a, ok := n.conn.LocalAddr().(*net.UDPAddr); ok {
		return uint16(a.Port)
	}
	return 0
}

// tickUDPTest asks the next clients for the UDP test.
func (n *Node) tickUDPTest(now time.Time) {
	t := &n.udp
	if !t.running || !n.connected(now) || n.cfg.CheckUDP == nil {
		return
	}
	for _, a := range t.asked {
		if !a.answered && now.Sub(a.at) > udpTestAnswerWait {
			// It took the request and nothing came: that counts against.
			a.answered = true
			t.finished++
		}
	}
	if t.finished >= udpTestClients {
		t.running, t.firewalled, t.verified = false, true, true
		return
	}
	if now.Sub(t.start) > udpTestTimeout {
		// No result in 6 minutes. As eMule, that only says firewalled when
		// no test ever gave a result and the TCP port is firewalled too;
		// otherwise the last result stays (a recheck that could not ask
		// anyone does not undo one that worked).
		t.running = false
		if !t.verified && n.firewalled() {
			t.firewalled, t.verified = true, true
		} else {
			t.firewalled = t.lastFW
		}
		return
	}
	if n.findingExtPort() {
		// Our external port first, from the pongs of a few nodes.
		if now.Sub(t.searchedAt) > 15*time.Second {
			t.searchedAt = now
			if c := n.table.RandomContact(3, 6); c != nil {
				n.sendTo(*c, OpPing, nil)
			}
		}
		return
	}
	inFlight := 0
	for _, a := range t.asked {
		if !a.answered {
			inFlight++
		}
	}
	if t.finished+inFlight >= udpTestClients {
		return
	}
	if len(t.candidates) == 0 {
		if _, ok := n.searches[n.udpTarget()]; !ok && now.Sub(t.searchedAt) > 15*time.Second {
			// Nodes we never talked to: those a lookup's answers name.
			t.searchedAt = now
			n.startSearch(searchNodeFWCheck, RandomID(), now, nil)
		}
		return
	}
	for len(t.candidates) > 0 {
		c := t.candidates[0]
		t.candidates = t.candidates[1:]
		if c.Version <= 5 || c.IP == n.publicIP() || c.ID == n.keys.KadID || n.table.GetByAddr(c.IP, 0) != nil {
			continue
		}
		if _, used := t.asked[c.IP]; used || !GoodAddr(c.IP, c.TCPPort) {
			continue
		}
		// Asked over TCP outside the node's lock (the eD2K side takes its
		// own); FailedUDPTest takes it back if it cannot be done.
		t.asked[c.IP] = &udpAsk{at: now}
		check, ip, tcp, intern, extern, key := n.cfg.CheckUDP, c.IP, c.TCPPort, n.localPort(), n.extPort, n.keys.VerifyKey(c.IP)
		n.later(func() {
			if !check(ip, tcp, intern, extern, key) {
				n.FailedUDPTest(ip)
			}
		})
		return
	}
}

func (n *Node) udpTarget() ID {
	for id, s := range n.searches {
		if s.typ == searchNodeFWCheck {
			return id
		}
	}
	return ID{}
}

// onFirewallUDP takes the answer of a client we asked for the UDP test.
func (n *Node) onFirewallUDP(in incoming, now time.Time) error {
	if len(in.payload) < 3 || !in.validKey {
		return errBad // the key we gave it proves the answer is its own
	}
	t := &n.udp
	a := t.asked[in.from.Addr()]
	if a == nil || a.answered || !t.running {
		return errBad
	}
	a.answered = true
	r := proto.NewReader(in.payload)
	code := r.U8()
	port := r.U16()
	intern := n.localPort()
	if port == 0 || (port != intern && port != n.extPort) || code != 0 {
		return nil // cancelled: the client knew us already, or another port
	}
	t.finished++
	t.running, t.firewalled, t.verified = false, false, true
	t.useIntern = port == intern && port != n.extPort && n.extPort != 0
	return nil
}

// FailedUDPTest is called when a client asked for the UDP test could not be
// asked after all (no TCP connection, or it does not do the test): it does
// not count, and another is asked.
func (n *Node) FailedUDPTest(ip netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if a := n.udp.asked[ip.Unmap()]; a != nil && !a.answered {
		a.answered = true
	}
}

// TestUDPFor answers a client's request for the UDP test (it sent us
// OP_FWCHECKUDPREQ over TCP, so its address is real): KADEMLIA2_FIREWALLUDP
// to its local and external Kad ports, error 1 if we know it already (the
// packet would not prove anything).
func (n *Node) TestUDPFor(ip netip.Addr, payload []byte) {
	r := proto.NewReader(payload)
	intern, extern, key := r.U16(), r.U16(), r.U32()
	if r.Err() != nil || intern == 0 {
		return
	}
	ip = ip.Unmap()
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	if !GoodAddr(ip, intern) || n.isBanned(ip, now) || !n.allowed(ip, OpFWCheckUDPReq, true, now) {
		return
	}
	known := n.table.GetByAddr(ip, 0) != nil
	k := UDPKey{Key: key, IP: n.publicIP()}.For(n.publicIP())
	ports := []uint16{intern}
	if extern != 0 && extern != intern {
		ports = append(ports, extern)
	}
	for _, port := range ports {
		var w proto.Writer
		if known {
			w.U8(1)
		} else {
			w.U8(0)
		}
		w.U16(port)
		n.sendUntracked(netip.AddrPortFrom(ip, port), OpFirewallUDP, w.B, ID{}, k)
	}
}

// ---- buddies ----

// needBuddy reports whether we look for a buddy: firewalled on TCP and
// UDP (with UDP open, direct callbacks do instead).
func (n *Node) needBuddy() bool {
	return n.firewalled() && n.udp.verified && n.udpFirewalled()
}

// buddyID is the ID our buddy is found by: the opposite of ours.
func (n *Node) buddyID() ID {
	var id ID
	for i := range id {
		id[i] = ^n.keys.KadID[i]
	}
	return id
}

func (n *Node) tickBuddy(now time.Time) {
	if n.mine != nil && !n.mine.connected && now.After(n.mine.until) {
		n.mine = nil // the connection never came up
	}
	for ip, b := range n.buddyOffers {
		if now.After(b.until) {
			delete(n.buddyOffers, ip)
		}
	}
	if !n.connected(now) || !n.needBuddy() || n.mine != nil || now.Before(n.nextFindBuddy) || n.cfg.ConnectBuddy == nil {
		return
	}
	n.nextFindBuddy = now.Add(findBuddyEvery)
	n.startSearch(searchFindBuddy, n.buddyID(), now, nil)
}

// storeFindBuddy asks a node near our buddy ID to be our buddy.
func (n *Node) storeFindBuddy(s *search, c Contact) {
	var w proto.Writer
	WriteID(&w, s.target)
	WriteID(&w, ID(n.keys.UserHash))
	w.U16(n.cfg.TCPPort())
	n.sendTo(c, OpFindBuddyReq, w.B)
	s.answers++
}

// onFindBuddyReq: a firewalled node asks us to be its buddy.
func (n *Node) onFindBuddyReq(in incoming, now time.Time) error {
	if len(in.payload) < 34 {
		return errBad
	}
	if n.firewalled() || !n.udpReachable() || n.cfg.TCPPort() == 0 {
		return nil // we could not help it
	}
	if n.served != nil {
		return nil // we have one
	}
	r := proto.NewReader(in.payload)
	id := ReadID(r)
	user := ReadID(r)
	tcp := r.U16()
	if tcp == 0 || !GoodAddr(in.from.Addr(), tcp) {
		return nil
	}
	// Several may wait to connect (one forged request must not keep the
	// place from real ones); the first that connects is served. Requests
	// without our key (they may be forged) take half the places at most;
	// one with it takes the place of the oldest of those when full.
	unproven := 0
	var oldest netip.Addr
	for ip, b := range n.buddyOffers {
		if now.After(b.until) {
			delete(n.buddyOffers, ip)
			continue
		}
		if !b.proven {
			unproven++
			if !oldest.IsValid() || b.until.Before(n.buddyOffers[oldest].until) {
				oldest = ip
			}
		}
	}
	if _, again := n.buddyOffers[in.from.Addr()]; !again {
		switch {
		case !in.validKey && unproven >= maxBuddyOffers/2:
			return nil
		case len(n.buddyOffers) >= maxBuddyOffers:
			if !in.validKey || !oldest.IsValid() {
				return nil
			}
			delete(n.buddyOffers, oldest)
		}
	}
	n.buddyOffers[in.from.Addr()] = &buddyLink{ip: in.from.Addr(), kadPort: in.from.Port(), tcpPort: tcp, userHash: user, id: id, proven: in.validKey, until: now.Add(buddyWait)}
	var w proto.Writer
	WriteID(&w, id)
	WriteID(&w, ID(n.keys.UserHash))
	w.U16(n.cfg.TCPPort())
	if in.senderKey.Key != 0 {
		w.U8(0) // connect options: no TCP obfuscation
	}
	n.reply(in, OpFindBuddyRes, w.B)
	return nil
}

// onFindBuddyRes: a node agrees to be our buddy; we connect to it.
func (n *Node) onFindBuddyRes(in incoming, now time.Time) error {
	if len(in.payload) < 34 || !n.wasAsked(in.from.Addr(), OpFindBuddyReq, now, false) {
		return errBad
	}
	r := proto.NewReader(in.payload)
	if ReadID(r) != n.buddyID() {
		return nil
	}
	user := ReadID(r)
	tcp := r.U16()
	if n.mine != nil || !n.needBuddy() || tcp == 0 || !GoodAddr(in.from.Addr(), tcp) || n.cfg.ConnectBuddy == nil {
		return nil
	}
	ip := in.from.Addr()
	n.mine = &buddyLink{ip: ip, kadPort: in.from.Port(), tcpPort: tcp, userHash: user, until: now.Add(buddyWait)}
	cb := n.cfg.ConnectBuddy
	n.later(func() {
		if !cb(ip, tcp, user) {
			n.BuddyDown(ip)
		}
	})
	return nil
}

// BuddyUp is called when the TCP connection to our buddy at ip is up;
// kadPort is the UDP port downloaders send it callback requests on.
func (n *Node) BuddyUp(ip netip.Addr, kadPort uint16) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.mine == nil || n.mine.ip != ip.Unmap() {
		return
	}
	n.mine.connected = true
	if kadPort != 0 {
		n.mine.kadPort = kadPort
	}
	clear(n.pub.srcNext) // republish our sources with the buddy's address
}

// BuddyDown is called when the connection to our buddy at ip ended.
func (n *Node) BuddyDown(ip netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.mine != nil && n.mine.ip == ip.Unmap() {
		n.mine = nil
		n.nextFindBuddy = time.Time{} // look for another
	}
}

// IncomingBuddy is called when a client connects to us over TCP: it
// reports whether it is the firewalled node we agreed to be the buddy of
// (same address and user hash). send then reaches it until ServedBuddyGone.
func (n *Node) IncomingBuddy(ip netip.Addr, userHash [16]byte, send func(op byte, payload []byte) bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.buddyOffers[ip.Unmap()]
	if n.served != nil || s == nil || s.userHash != userHash || n.now().After(s.until) {
		return false
	}
	s.connected, s.send = true, send
	n.served = s
	clear(n.buddyOffers)
	return true
}

// IsBuddyOffer reports whether a client connecting from ip with userHash
// is one we offered to be the buddy of (our hello then names our UDP port,
// which it gives downloaders).
func (n *Node) IsBuddyOffer(ip netip.Addr, userHash [16]byte) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.buddyOffers[ip.Unmap()]
	return n.served == nil && s != nil && s.userHash == userHash
}

// ServedBuddyGone is called when the node we are the buddy of disconnects.
func (n *Node) ServedBuddyGone(ip netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.served != nil && n.served.ip == ip.Unmap() {
		n.served = nil
	}
}

// onCallbackReq: a downloader asks us, the buddy, to have our firewalled
// node connect to it.
func (n *Node) onCallbackReq(in incoming) error {
	if len(in.payload) < 34 {
		return errBad
	}
	s := n.served
	if s == nil || !s.connected {
		return nil
	}
	r := proto.NewReader(in.payload)
	check := ReadID(r)
	file := ReadID(r)
	tcp := r.U16()
	if check != s.id || tcp == 0 || !GoodAddr(in.from.Addr(), tcp) {
		return nil
	}
	if !n.relayAllowed(n.now()) {
		return nil
	}
	var w proto.Writer
	WriteID(&w, check)
	WriteID(&w, file)
	w.U32(ipToU32(in.from.Addr()))
	w.U16(tcp)
	send := s.send
	n.later(func() { send(OpCallback, w.B) })
	return nil
}

// relayAllowed limits what we pass to the node we serve, whatever the
// senders (their addresses may be forged): maxRelaysPerMin.
func (n *Node) relayAllowed(now time.Time) bool {
	kept := n.relays[:0]
	for _, t := range n.relays {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	n.relays = kept
	if len(n.relays) >= maxRelaysPerMin {
		return false
	}
	n.relays = append(n.relays, now)
	return true
}

// Callback reads an OP_CALLBACK our buddy passed on: the downloader at
// ip:tcpPort wants file from us. ok is false if it was not meant for us.
func (n *Node) Callback(payload []byte) (ip netip.Addr, tcpPort uint16, file [16]byte, ok bool) {
	r := proto.NewReader(payload)
	check := ReadID(r)
	f := ReadID(r)
	ip = ipFromU32(r.U32())
	tcpPort = r.U16()
	if r.Err() != nil {
		return ip, 0, file, false
	}
	n.mu.Lock()
	mine := n.buddyID()
	n.mu.Unlock()
	if check != mine || tcpPort == 0 || !GoodAddr(ip, tcpPort) {
		return ip, 0, file, false
	}
	return ip, tcpPort, [16]byte(f), true
}

// onClientUDP handles the eD2K client UDP packets (0xC5) of the buddy
// system and direct callbacks.
func (n *Node) onClientUDP(data []byte, from netip.AddrPort, now time.Time) {
	if len(data) < 2 || !n.allowed(from.Addr(), 0xC5, false, now) {
		return
	}
	op, p := data[1], data[2:]
	switch op {
	case OpReaskCallbackUDP:
		// To the node we are the buddy of, with the asker's address.
		s := n.served
		if s == nil || !s.connected || len(p) < 17 || len(p) > maxReaskBytes || ID([16]byte(p[:16])) != s.id || !n.relayAllowed(now) {
			return
		}
		out := append([]byte(nil), p[10:]...)
		a := from.Addr().As4()
		copy(out[0:4], a[:])
		binary.LittleEndian.PutUint16(out[4:6], from.Port())
		send := s.send
		n.later(func() { send(OpReaskCallbackTCP, out) })
	case OpDirectCallback:
		// We are firewalled on TCP but reachable over UDP: connect to the
		// downloader that asks.
		if len(p) < 19 || !n.firewalled() || !n.udpReachable() || n.cfg.ConnectTo == nil {
			return
		}
		ip := from.Addr()
		if at, ok := n.directAsked[ip]; ok && now.Sub(at) < directCallbackEvery {
			return
		}
		tcp := binary.LittleEndian.Uint16(p)
		if tcp == 0 || !GoodAddr(ip, tcp) {
			return
		}
		// Each address once per 3 minutes; the eD2K side's callback gate
		// limits how many in all. The addresses remembered are limited
		// too (forged ones cost nothing): when full, one is forgotten.
		for k := range n.directAsked {
			if len(n.directAsked) < maxDirectAsked {
				break
			}
			delete(n.directAsked, k)
		}
		n.directAsked[ip] = now
		connect := n.cfg.ConnectTo
		n.later(func() { connect(ip, tcp) })
	}
}

// RequestCallback asks the buddy of a firewalled source to have it connect
// to us (we want file from it). Without the buddy's address, the nodes
// near the buddy ID are asked.
func (n *Node) RequestCallback(buddyIP netip.Addr, buddyPort uint16, buddyID ID, file [16]byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.connected(n.now()) {
		return ErrNotConnected
	}
	var w proto.Writer
	WriteID(&w, buddyID)
	WriteID(&w, ID(file))
	w.U16(n.cfg.TCPPort())
	if GoodAddr(buddyIP, buddyPort) {
		n.send(netip.AddrPortFrom(buddyIP.Unmap(), buddyPort), OpCallbackReq, w.B, ID{}, 0)
		return nil
	}
	if _, ok := n.searches[buddyID]; ok {
		return ErrBusy
	}
	running := 0
	for _, s := range n.searches {
		if s.typ == searchFindSource {
			running++
		}
	}
	if running >= maxCallbackLookups {
		return ErrBusy
	}
	n.startSearch(searchFindSource, buddyID, n.now(), func(s *search) { s.files = [][16]byte{file} })
	return nil
}

// maxCallbackLookups is how many lookups for buddies run at once: each
// sends dozens of packets, and sources naming a buddy without its address
// cost an attacker nothing.
const maxCallbackLookups = 5

// storeFindSource asks a node near a buddy ID to pass our callback on.
func (n *Node) storeFindSource(s *search, c Contact) {
	if len(s.files) != 1 {
		return
	}
	var w proto.Writer
	WriteID(&w, s.target)
	WriteID(&w, ID(s.files[0]))
	w.U16(n.cfg.TCPPort())
	n.sendTo(c, OpCallbackReq, w.B)
	s.answers++
}

// DirectCallback asks a source that is firewalled on TCP but reachable
// over UDP (type 6) to connect to us; not when we are firewalled ourselves
// (it could not).
func (n *Node) DirectCallback(ip netip.Addr, udpPort uint16) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !GoodAddr(ip, udpPort) || n.cfg.TCPPort() == 0 || n.firewalled() {
		return ErrFirewalled
	}
	var w proto.Writer
	w.U8(ProtoEMule)
	w.U8(OpDirectCallback)
	w.U16(n.cfg.TCPPort())
	w.Bytes(n.keys.UserHash[:])
	w.U8(0) // connect options
	n.conn.WriteTo(w.B, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip.Unmap(), udpPort)))
	return nil
}

// ErrFirewalled is returned for callbacks that need others to reach us.
var ErrFirewalled = errors.New("kad: firewalled")

// ConnectDirectCallback is the connect option (TAG_ENCRYPTION) of a source
// that takes direct callbacks.
const ConnectDirectCallback = 0x08

// FirewallState is what the eD2K side tells peers about us.
type FirewallState struct {
	BuddyIP        netip.Addr // our buddy, when firewalled
	BuddyPort      uint16
	DirectCallback bool // firewalled on TCP, reachable over UDP
}

// Firewall returns our firewall state for the eD2K hello.
func (n *Node) Firewall() FirewallState {
	n.mu.Lock()
	defer n.mu.Unlock()
	var s FirewallState
	if n.firewalled() {
		if n.mine != nil && n.mine.connected {
			s.BuddyIP, s.BuddyPort = n.mine.ip, n.mine.kadPort
		}
		s.DirectCallback = n.udpReachable()
	}
	return s
}

// sourceTags are the tags of our source publishes, or nil when downloaders
// could not reach us.
func (n *Node) sourceTags(f SharedFile) []proto.Tag {
	tcp := n.cfg.TCPPort()
	if tcp == 0 {
		return nil
	}
	large := f.Size > largeFileSize
	var tags []proto.Tag
	switch {
	case !n.firewalled():
		typ := uint64(1)
		if large {
			typ = 4
		}
		tags = []proto.Tag{IntTag(TagSourceType, typ), IntTag(TagSourcePort, uint64(tcp))}
	case n.udpReachable():
		tags = []proto.Tag{IntTag(TagSourceType, 6), IntTag(TagSourcePort, uint64(tcp))}
	case n.mine != nil && n.mine.connected:
		typ := uint64(3)
		if large {
			typ = 5
		}
		a := n.mine.ip.As4()
		bid := n.buddyID()
		tags = []proto.Tag{
			IntTag(TagSourceType, typ),
			IntTag(TagServerIP, uint64(binary.LittleEndian.Uint32(a[:]))), // network order, as eMule
			IntTag(TagServerPort, uint64(n.mine.kadPort)),
			StrTag(TagBuddyHash, strings.ToUpper(hex.EncodeToString(bid[:]))),
			IntTag(TagSourcePort, uint64(tcp)),
		}
	default:
		return nil
	}
	if n.udp.useIntern {
		tags = append(tags, IntTag(TagSourceUDPPort, uint64(n.localPort())))
	}
	// Connect options: our TCP obfuscation, and 0x08 = we take direct
	// callbacks, which eMule requires of type 6 sources.
	opts := uint64(n.cfg.Crypt & 0x07)
	if n.firewalled() && n.udpReachable() {
		opts = ConnectDirectCallback
	}
	tags = append(tags, IntTag(TagFileSize, f.Size), IntTag(TagEncryption, opts))
	return tags
}
