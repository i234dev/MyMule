package kad

import (
	"crypto/rand"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/proto"
)

// fakeNet carries datagrams between in-memory nodes.
type fakeNet struct {
	nodes map[netip.AddrPort]*Node
	queue []dgram
	now   time.Time
	// sent records datagrams to addresses without a node; all, every
	// datagram delivered.
	sent map[netip.AddrPort][][]byte
	all  []dgram
}

type dgram struct {
	from, to netip.AddrPort
	b        []byte
}

type fakeConn struct {
	fn   *fakeNet
	addr netip.AddrPort
}

func (c *fakeConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	ap := addr.(*net.UDPAddr).AddrPort()
	c.fn.queue = append(c.fn.queue, dgram{c.addr, netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), append([]byte(nil), b...)})
	return len(b), nil
}
func (c *fakeConn) ReadFrom([]byte) (int, net.Addr, error) { select {} }
func (c *fakeConn) Close() error                           { return nil }
func (c *fakeConn) LocalAddr() net.Addr                    { return net.UDPAddrFromAddrPort(c.addr) }
func (c *fakeConn) SetDeadline(time.Time) error            { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error        { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error       { return nil }

func newFakeNet() *fakeNet {
	return &fakeNet{nodes: map[netip.AddrPort]*Node{}, now: t0, sent: map[netip.AddrPort][][]byte{}}
}

func (fn *fakeNet) node(t testing.TB, i int, cfg Config) *Node {
	addr := netip.AddrPortFrom(pubIP(i, false), 4672)
	cfg.Conn = &fakeConn{fn, addr}
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	cfg.Now = func() time.Time { return fn.now }
	cfg.UserHash = [16]byte{byte(i), byte(i >> 8), 0xEE}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.saves.Wait) // before the folder goes: saves run in the background
	fn.nodes[addr] = n
	return n
}

func addrOf(n *Node) netip.AddrPort {
	return n.conn.(*fakeConn).addr
}

func contactOf(n *Node) Contact {
	a := addrOf(n)
	return Contact{ID: n.ID(), IP: a.Addr(), UDPPort: a.Port(), TCPPort: 4662, Version: Version, Verified: true}
}

// pump delivers queued datagrams until none are left.
func (fn *fakeNet) pump() {
	for i := 0; i < 100000 && len(fn.queue) > 0; i++ {
		d := fn.queue[0]
		fn.queue = fn.queue[1:]
		fn.all = append(fn.all, d)
		n := fn.nodes[d.to]
		if n == nil {
			fn.sent[d.to] = append(fn.sent[d.to], d.b)
			continue
		}
		n.mu.Lock()
		n.handle(d.b, d.from, fn.now)
		n.mu.Unlock()
	}
}

// inject sends a datagram from an address without a node.
func (fn *fakeNet) inject(from netip.AddrPort, to *Node, b []byte) {
	fn.queue = append(fn.queue, dgram{from, addrOf(to), b})
	fn.pump()
}

func locked[T any](n *Node, f func() T) T {
	n.mu.Lock()
	defer n.mu.Unlock()
	return f()
}

// A HELLO exchange verifies both sides: A answers with B's key, and B
// asks A to acknowledge with A's key.
func TestNodeHelloVerifies(t *testing.T) {
	fn := newFakeNet()
	a, b := fn.node(t, 1, Config{}), fn.node(t, 2, Config{})
	a.mu.Lock()
	a.sendHello(OpHelloReq, addrOf(b), Version, 0, b.ID(), false)
	a.mu.Unlock()
	fn.pump()
	inA := locked(a, func() *Contact { return a.table.Get(b.ID()) })
	inB := locked(b, func() *Contact { return b.table.Get(a.ID()) })
	if inA == nil || inB == nil {
		t.Fatalf("not added: %v %v", inA, inB)
	}
	if !inA.Verified || !inB.Verified {
		t.Fatalf("verified: in A %v, in B %v", inA.Verified, inB.Verified)
	}
	if inA.Key.Key == 0 || inB.Key.Key == 0 {
		t.Fatal("verify keys not kept")
	}
	if !locked(a, func() bool { return a.connected(fn.now) }) {
		t.Fatal("not connected after an answer")
	}
}

func TestNodeBootstrap(t *testing.T) {
	fn := newFakeNet()
	a, b := fn.node(t, 1, Config{}), fn.node(t, 2, Config{})
	for i := 10; i < 40; i++ {
		b.table.Add(contactN(i), false, false, t0)
	}
	a.Bootstrap(addrOf(b))
	fn.pump()
	n := locked(a, func() int { return a.table.Len() })
	if n < 21 || locked(a, func() *Contact { return a.table.Get(b.ID()) }) == nil {
		t.Fatalf("%d contacts after bootstrap", n)
	}
	// An answer nobody asked for is ignored.
	c := fn.node(t, 3, Config{})
	c.mu.Lock()
	c.onBootstrapReq(incoming{from: addrOf(a)})
	c.mu.Unlock()
	fn.pump()
	if locked(a, func() *Contact { return a.table.Get(c.ID()) }) != nil {
		t.Fatal("unrequested bootstrap answer used")
	}
}

// A full lookup through a network of 60 nodes finds the node closest to
// the target, starting from 5 contacts.
func TestNodeLookup(t *testing.T) {
	fn := newFakeNet()
	var all []*Node
	for i := 1; i <= 60; i++ {
		all = append(all, fn.node(t, i, Config{}))
	}
	for _, n := range all {
		for _, o := range all {
			if o != n {
				n.table.Add(contactOf(o), false, false, t0)
				n.table.SetAlive(o.ID(), t0)
			}
		}
	}
	a := fn.node(t, 100, Config{})
	for _, o := range all[:5] {
		a.table.Add(contactOf(o), false, false, t0)
		a.table.SetAlive(o.ID(), t0)
	}
	target := RandomID()
	closest := all[0]
	for _, o := range all {
		if o.ID().Xor(target).Cmp(closest.ID().Xor(target)) < 0 {
			closest = o
		}
	}
	a.mu.Lock()
	s := a.startSearch(searchNodeComplete, target, fn.now, nil)
	a.mu.Unlock()
	fn.pump()
	for i := 0; i < 40; i++ {
		fn.now = fn.now.Add(time.Second)
		a.mu.Lock()
		a.tickSearches(fn.now)
		a.mu.Unlock()
		fn.pump()
	}
	if _, ok := s.tried[closest.ID().Xor(target)]; !ok {
		t.Fatalf("closest node never asked (asked %d)", len(s.tried))
	}
	if _, ok := s.responded[closest.ID().Xor(target)]; !ok {
		t.Fatal("closest node's answer not processed")
	}
	// The nodes it met near the target are now in the table.
	if locked(a, func() *Contact { return a.table.Get(closest.ID()) }) == nil {
		t.Fatal("closest node not added to the table")
	}
	// The search ends.
	fn.now = fn.now.Add(time.Minute)
	a.mu.Lock()
	a.tickSearches(fn.now)
	left := len(a.searches)
	a.mu.Unlock()
	if left != 0 {
		t.Fatal("search never ended")
	}
}

func rawPacket(op byte, payload []byte) []byte { return Packet{Opcode: op, Payload: payload}.Encode() }

func TestNodeUnrequestedAnswers(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }})
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	var w proto.Writer
	WriteID(&w, RandomID())
	w.U8(1)
	c := contactN(60)
	writeContact(&w, c)
	fn.inject(x, a, rawPacket(OpRes, w.B))
	if a.table.Get(c.ID) != nil {
		t.Fatal("contact from an unrequested KADEMLIA2_RES added")
	}
	var w2 proto.Writer
	w2.U32(0x01020304)
	fn.inject(x, a, rawPacket(OpFirewalledRes, w2.B))
	if a.extIP.IsValid() || a.recheckIP != 0 {
		t.Fatal("unrequested firewall answer used")
	}
	fn.inject(x, a, rawPacket(OpFirewalledAckRes, nil))
	if a.TCPCheckAck(x.Addr()) || a.firewalledN != 0 {
		t.Fatal("unrequested firewall ack counted")
	}
	var w3 proto.Writer
	w3.U16(1234)
	fn.inject(x, a, rawPacket(OpPong, w3.B))
	if len(a.extPortIPs) != 0 {
		t.Fatal("unrequested pong used")
	}
}

func TestNodeFloodBan(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	for range 11 {
		fn.inject(x, a, rawPacket(OpPing, nil))
	}
	if got := len(fn.sent[x]); got != 2 {
		t.Fatalf("%d pongs for 11 pings in a second (2 per minute allowed)", got)
	}
	if _, ok := a.banned[x.Addr()]; !ok {
		t.Fatal("flooder not banned")
	}
	fn.inject(x, a, rawPacket(OpBootstrapReq, nil))
	if len(fn.sent[x]) != 2 {
		t.Fatal("banned IP answered")
	}
	// The allowance comes back with time.
	y := netip.AddrPortFrom(pubIP(51, false), 4672)
	for i := range 6 {
		fn.now = fn.now.Add(31 * time.Second)
		fn.inject(y, a, rawPacket(OpPing, nil))
		if len(fn.sent[y]) != i+1 {
			t.Fatalf("ping %d at a normal rate refused", i)
		}
	}
	// The ban ends after an hour.
	fn.now = fn.now.Add(banTime + time.Second)
	a.tick(fn.now)
	fn.inject(x, a, rawPacket(OpPing, nil))
	if len(fn.sent[x]) != 3 {
		t.Fatal("ban never lifted")
	}
}

func TestNodeFirewallCheck(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }})
	type call struct {
		ip   netip.Addr
		port uint16
		user [16]byte
	}
	var calls []call
	helper := Config{ConnectBack: func(ip netip.Addr, port uint16, user [16]byte) bool {
		calls = append(calls, call{ip, port, user})
		return true
	}}
	b, c := fn.node(t, 2, helper), fn.node(t, 3, helper)
	if !a.Firewalled() {
		t.Fatal("open before any check")
	}
	for _, o := range []*Node{b, c} {
		a.mu.Lock()
		a.sendHello(OpHelloReq, addrOf(o), Version, 0, o.ID(), false)
		a.mu.Unlock()
		fn.pump()
	}
	if len(calls) != 2 || calls[0].ip != addrOf(a).Addr() || calls[0].port != 4662 || calls[0].user != a.keys.UserHash {
		t.Fatalf("connect-back calls: %+v", calls)
	}
	ip, _ := a.ExternalAddr()
	if ip != addrOf(a).Addr() || a.recheckIP != 2 {
		t.Fatalf("external IP %v, %d answers", ip, a.recheckIP)
	}
	if !a.TCPCheckAck(addrOf(b).Addr()) || a.TCPCheckAck(addrOf(b).Addr()) {
		t.Fatal("ack not counted exactly once")
	}
	if a.TCPCheckAck(pubIP(70, false)) {
		t.Fatal("ack from a node we did not ask counted")
	}
	if !a.Firewalled() {
		t.Fatal("open after one ack")
	}
	a.TCPCheckAck(addrOf(c).Addr())
	if a.Firewalled() {
		t.Fatal("still firewalled after two acks")
	}
	// The hourly recheck keeps the last answer while it runs.
	a.mu.Lock()
	a.recheckFirewalled(fn.now)
	a.mu.Unlock()
	if a.Firewalled() {
		t.Fatal("recheck forgot the last state")
	}
	// No TCP port: nobody is asked.
	d := fn.node(t, 4, Config{})
	calls = nil
	d.mu.Lock()
	d.sendHello(OpHelloReq, addrOf(b), Version, 0, b.ID(), false)
	d.mu.Unlock()
	fn.pump()
	if len(calls) != 0 || len(d.fwAsked) != 0 {
		t.Fatal("firewall check without a TCP port")
	}
	// One node's answer counts once.
	before := a.recheckIP
	a.fwAsked[addrOf(b).Addr()] = &fwRequest{at: fn.now, answered: true}
	var w proto.Writer
	w.U32(ipToU32(addrOf(a).Addr()))
	b.mu.Lock()
	b.send(addrOf(a), OpFirewalledRes, w.B, ID{}, a.keys.VerifyKey(addrOf(b).Addr()))
	b.mu.Unlock()
	fn.pump()
	if a.recheckIP != before {
		t.Fatal("a node's firewall answer counted twice")
	}
	// A plain answer (no key: the source address may be forged) is not
	// used.
	a.fwAsked[addrOf(b).Addr()] = &fwRequest{at: fn.now, op: OpFirewalled2Req}
	b.mu.Lock()
	b.send(addrOf(a), OpFirewalledRes, w.B, ID{}, 0)
	b.mu.Unlock()
	fn.pump()
	if a.recheckIP != before {
		t.Fatal("firewall answer without our key used")
	}
	// The UDP ack is only for requests of the old kind.
	b.mu.Lock()
	b.send(addrOf(a), OpFirewalledAckRes, nil, ID{}, 0)
	b.mu.Unlock()
	fn.pump()
	if a.fwAsked[addrOf(b).Addr()].acked {
		t.Fatal("UDP ack accepted for a FIREWALLED2 request")
	}
	a.fwAsked[addrOf(b).Addr()].op = OpFirewalledReq
	b.mu.Lock()
	b.send(addrOf(a), OpFirewalledAckRes, nil, ID{}, 0)
	b.mu.Unlock()
	fn.pump()
	if !a.fwAsked[addrOf(b).Addr()].acked {
		t.Fatal("UDP ack refused for an old request")
	}
}

// A HELLO without our key (its source address may be forged) does not
// make us ask that address for a firewall check.
func TestNodeNoFirewallCheckForUnprovenHello(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }})
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	var w proto.Writer
	WriteID(&w, RandomID())
	w.U16(4662)
	w.U8(8)
	w.U8(0)
	fn.inject(x, a, rawPacket(OpHelloReq, w.B))
	if len(a.fwAsked) != 0 {
		t.Fatal("firewall check asked of an unproven address")
	}
}

// Contacts that answered our HELLO count as having said hello: legacy
// ones may then not move.
func TestNodeHelloResMarksHello(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	id := RandomID()
	a.outTracked = append(a.outTracked, tracked{ip: x.Addr(), op: OpHelloReq, at: fn.now})
	var w proto.Writer
	WriteID(&w, id)
	w.U16(4662)
	w.U8(5)
	w.U8(0)
	fn.inject(x, a, rawPacket(OpHelloRes, w.B))
	c := a.table.Get(id)
	if c == nil || !c.helloReceived {
		t.Fatalf("contact %+v", c)
	}
}

// A bootstrap nodes.dat gives the 50 contacts closest to our ID.
func TestBootstrapList(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	var cs []Contact
	for i := 10; i < 300; i++ {
		cs = append(cs, contactN(i))
	}
	var w proto.Writer
	w.U32(0)
	w.U32(3)
	w.U32(1)
	w.U32(uint32(len(cs)))
	for _, c := range cs {
		writeContact(&w, c)
	}
	if _, err := a.AddNodes(w.B); err != nil {
		t.Fatal(err)
	}
	a.AddNodes(w.B) // twice: still 50
	if len(a.bootstrapList) != maxBootstrap || a.table.Len() != 0 {
		t.Fatalf("%d to bootstrap, %d in the table", len(a.bootstrapList), a.table.Len())
	}
	me := a.ID()
	far := 0
	for _, c := range cs {
		if c.ID.Xor(me).Cmp(a.bootstrapList[maxBootstrap-1].ID.Xor(me)) <= 0 {
			far++
		}
	}
	if far != maxBootstrap {
		t.Fatalf("not the closest: %d contacts at or below the farthest kept", far)
	}
	// They are asked one at a time while we are not connected.
	fn.now = fn.now.Add(time.Minute)
	a.tick(fn.now)
	a.tick(fn.now.Add(time.Second))
	if len(a.bootstrapList) != maxBootstrap-1 {
		t.Fatalf("%d left", len(a.bootstrapList))
	}
}

// A node too old for verify keys proves its IP by answering a
// KADEMLIA2_REQ with a random target sent to that IP.
func TestNodeLegacyChallenge(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	xid := RandomID()
	var w proto.Writer
	WriteID(&w, xid)
	w.U16(4662)
	w.U8(5)
	w.U8(0)
	fn.inject(x, a, rawPacket(OpHelloReq, w.B))
	c := a.table.Get(xid)
	if c == nil || c.Verified {
		t.Fatalf("legacy contact: %+v", c)
	}
	var chal ID
	for _, b := range fn.sent[x] {
		if p, err := DecodePacket(b); err == nil && p.Opcode == OpReq {
			r := proto.NewReader(p.Payload)
			r.U8()
			chal = ReadID(r)
		}
	}
	if chal.IsZero() {
		t.Fatal("no challenge sent")
	}
	// A wrong answer does not verify it; the right one does.
	var bad proto.Writer
	WriteID(&bad, RandomID())
	bad.U8(0)
	fn.inject(x, a, rawPacket(OpRes, bad.B))
	if c.Verified {
		t.Fatal("verified by a wrong answer")
	}
	// (The wrong answer used up the tracked request: ask again.)
	a.mu.Lock()
	a.outTracked = append(a.outTracked, tracked{ip: x.Addr(), op: OpReq, at: fn.now})
	a.mu.Unlock()
	var good proto.Writer
	WriteID(&good, chal)
	good.U8(0)
	fn.inject(x, a, rawPacket(OpRes, good.B))
	if !c.Verified {
		t.Fatal("not verified by the right answer")
	}
}

func TestExternalPort(t *testing.T) {
	n := &Node{}
	n.setExtPort(5000, pubIP(1, false))
	n.setExtPort(5000, pubIP(1, false)) // same node again: ignored
	if n.extPort != 0 || !n.findingExtPort() {
		t.Fatal("one answer was enough")
	}
	n.setExtPort(5000, pubIP(2, false))
	if n.extPort != 5000 || n.findingExtPort() {
		t.Fatalf("two agreeing answers: port %d", n.extPort)
	}
	m := &Node{}
	m.setExtPort(1, pubIP(1, false))
	m.setExtPort(2, pubIP(2, false))
	m.setExtPort(3, pubIP(3, false))
	if m.extPort != 0 || m.findingExtPort() {
		t.Fatal("three different answers gave a port")
	}
}

func TestExternalIPNeedsTwo(t *testing.T) {
	n := &Node{}
	a, b := pubIP(1, false), pubIP(2, false)
	n.setExtIP(a)
	if n.extIP != a {
		t.Fatal("first answer not taken")
	}
	n.setExtIP(b)
	if n.extIP != a {
		t.Fatal("changed by one answer")
	}
	n.setExtIP(b)
	if n.extIP != b {
		t.Fatal("not changed by two")
	}
}

func TestNodeStatePersists(t *testing.T) {
	dir := t.TempDir()
	fn := newFakeNet()
	a := fn.node(t, 1, Config{Dir: dir})
	b := fn.node(t, 2, Config{Dir: dir})
	if a.ID() != b.ID() || a.keys.UDPKey != b.keys.UDPKey || a.keys.UDPKey == 0 {
		t.Fatal("identity not kept")
	}
	os.WriteFile(filepath.Join(dir, stateFile), []byte{1, 0, 0, 0, 5}, 0o600)
	var logged bool
	c := fn.node(t, 3, Config{Dir: dir, Log: func(string, ...any) { logged = true }})
	if c.ID() == a.ID() || !logged {
		t.Fatal("broken kad.dat not replaced")
	}
	// nodes.dat: saved with 25 contacts or more, and read back.
	for i := 10; i < 60; i++ {
		c.table.Add(contactN(i), false, false, t0)
	}
	if err := c.SaveNodes(); err != nil {
		t.Fatal(err)
	}
	d := fn.node(t, 4, Config{Dir: dir})
	if d.table.Len() < 25 {
		t.Fatalf("%d contacts read back", d.table.Len())
	}
}

// Random datagrams of every opcode never panic.
func TestNodeHostileInput(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{TCPPort: func() uint16 { return 4662 }, ConnectBack: func(netip.Addr, uint16, [16]byte) bool { return true }})
	for i := 10; i < 40; i++ {
		a.table.Add(contactN(i), false, false, t0)
	}
	ops := []byte{OpBootstrapReq, OpBootstrapRes, OpHelloReq, OpHelloRes, OpHelloResAck, OpReq, OpRes,
		OpPing, OpPong, OpFirewalledReq, OpFirewalled2Req, OpFirewalledRes, OpFirewalledAckRes, 0x99}
	buf := make([]byte, 120)
	for i := range 20000 {
		op := ops[i%len(ops)]
		rand.Read(buf)
		n := int(buf[0]) % len(buf)
		from := netip.AddrPortFrom(pubIP(1000+i%300, false), uint16(1+i%5000))
		// Make answers look requested.
		a.outTracked = append(a.outTracked, tracked{ip: from.Addr(), op: OpReq, at: fn.now}, tracked{ip: from.Addr(), op: OpHelloReq, at: fn.now})
		a.fwAsked[from.Addr()] = &fwRequest{at: fn.now}
		fn.queue = append(fn.queue, dgram{from, addrOf(a), rawPacket(op, buf[:n])})
		fn.pump()
		fn.now = fn.now.Add(time.Second)
	}
}

// A HELLO_RES_ACK only verifies a contact when it carries our key.
func TestNodeAckNeedsKey(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	c := Contact{ID: RandomID(), IP: x.Addr(), UDPPort: x.Port(), Version: 8}
	a.table.Add(c, false, false, t0)
	a.outTracked = append(a.outTracked, tracked{ip: x.Addr(), op: OpHelloRes, at: fn.now})
	var w proto.Writer
	WriteID(&w, c.ID)
	w.U8(0)
	fn.inject(x, a, rawPacket(OpHelloResAck, w.B))
	if a.table.Get(c.ID).Verified {
		t.Fatal("verified by a plain ack")
	}
	a.outTracked = append(a.outTracked, tracked{ip: x.Addr(), op: OpHelloRes, at: fn.now})
	fn.inject(x, a, a.keys.EncryptKad(rawPacket(OpHelloResAck, w.B), x.Addr(), ID{}, a.keys.VerifyKey(x.Addr())))
	if !a.table.Get(c.ID).Verified {
		t.Fatal("not verified by an ack with our key")
	}
}

// KADEMLIA2_REQ is answered only when addressed to our ID.
func TestNodeReqForOtherNode(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	x := netip.AddrPortFrom(pubIP(50, false), 4672)
	var w proto.Writer
	w.U8(findNode)
	WriteID(&w, RandomID())
	WriteID(&w, RandomID())
	fn.inject(x, a, rawPacket(OpReq, w.B))
	if len(fn.sent[x]) != 0 {
		t.Fatal("answered a request for another node")
	}
	var w2 proto.Writer
	w2.U8(findNode)
	WriteID(&w2, RandomID())
	WriteID(&w2, a.ID())
	fn.inject(x, a, rawPacket(OpReq, w2.B))
	if len(fn.sent[x]) != 1 {
		t.Fatal("request for us not answered")
	}
}

// Answers with more contacts than asked for, and several contacts of one
// IP or /24, are not followed.
func TestSearchResponseFilters(t *testing.T) {
	fn := newFakeNet()
	a := fn.node(t, 1, Config{})
	target := RandomID()
	s := newSearch(searchNodeComplete, target, fn.now)
	from := contactN(1)
	s.tried[from.ID.Xor(target)] = from
	var many []Contact
	for i := 2; i < 2+findNode+1; i++ {
		many = append(many, contactN(i))
	}
	a.processResponse(s, from.Addr(), many, fn.now)
	if len(s.possible) != 0 {
		t.Fatal("answer with too many contacts followed")
	}
	var same []Contact
	for i := 1; i <= 5; i++ {
		c := contactN(100)
		c.IP = pubIP(i, true) // one /24
		same = append(same, c)
	}
	dup := contactN(200)
	same = append(same, dup, dup)
	a.processResponse(s, from.Addr(), same, fn.now)
	if len(s.possible) != 3 { // two of the /24 and one of the duplicated IP
		t.Fatalf("%d contacts followed", len(s.possible))
	}
}
