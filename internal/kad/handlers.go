package kad

import (
	"errors"
	"net/netip"
	"time"

	"mymule/internal/proto"
)

// Requests whose answers we only accept from nodes we sent them to.
func isTrackedRequest(op byte) bool {
	switch op {
	case OpBootstrapReq, OpHelloReq, OpHelloRes, OpReq, OpSearchNotesReq,
		OpPublishKeyReq, OpPublishSourceReq, OpPublishNotesReq,
		OpFindBuddyReq, OpCallbackReq, OpPing:
		return true
	}
	return false
}

type tracked struct {
	ip netip.Addr
	op byte
	at time.Time
}

// wasAsked reports whether we sent op to ip in the last 3 minutes, and
// forgets it unless keep.
func (n *Node) wasAsked(ip netip.Addr, op byte, now time.Time, keep bool) bool {
	for i := len(n.outTracked) - 1; i >= 0; i-- {
		t := n.outTracked[i]
		if now.Sub(t.at) > trackWindow {
			n.outTracked = append(n.outTracked[:0], n.outTracked[i+1:]...)
			return false
		}
		if t.ip == ip && t.op == op {
			if !keep {
				n.outTracked = append(n.outTracked[:i], n.outTracked[i+1:]...)
			}
			return true
		}
	}
	return false
}

// Flood control (eMule's InTrackListIsAllowedPacket): how many requests
// of each kind one IP may send per minute. Beyond 5 times that, it is
// banned for an hour, but only if it proved its address (our key): a
// forged one must not get someone else banned.
//
// Each address seen costs memory, and forged ones cost an attacker
// nothing, so the state is limited: maxTrackedIPs addresses (beyond that,
// limited requests from new addresses are refused until old ones expire),
// maxBanned bans, all cleaned every minute.
func allowedPerMinute(op byte) int {
	switch op {
	case OpBootstrapReq, OpPublishSourceReq, OpPublishNotesReq, OpFirewalledReq, OpFirewalled2Req, OpFindBuddyReq, OpPing:
		return 2
	case OpFWCheckUDPReq: // over TCP: UDP firewall tests we run for others
		return 1
	case ProtoEMule: // eD2K client UDP on the Kad port
		return 10
	case OpHelloReq, OpSearchNotesReq, OpSearchKeyReq, OpSearchSourceReq, OpPublishKeyReq:
		return 3
	case OpReq:
		return 10
	case OpCallbackReq:
		return 1
	}
	return 0
}

const (
	maxTrackedIPs  = 20000
	maxBanned      = 10000
	maxDirectAsked = 4096
	floodCleanup   = time.Minute
)

type floodEntry struct {
	ops     []floodCount
	expires time.Time
}

type floodCount struct {
	op    byte
	count int
	first time.Time
}

// cleanFlood drops the flood state that expired.
func (n *Node) cleanFlood(now time.Time) {
	n.inCleanup = now
	for k, e := range n.inTracked {
		if e.expires.Before(now) {
			delete(n.inTracked, k)
		}
	}
	for ip, until := range n.banned {
		if !until.After(now) {
			delete(n.banned, ip)
		}
	}
	for ip, at := range n.directAsked {
		if now.Sub(at) > directCallbackEvery {
			delete(n.directAsked, ip)
		}
	}
}

// isBanned reports whether ip is banned now.
func (n *Node) isBanned(ip netip.Addr, now time.Time) bool {
	until, ok := n.banned[ip]
	return ok && until.After(now)
}

// allowed counts a request of op from ip and reports whether it may be
// served. proven: the address is not forged (our key, or a TCP
// connection).
func (n *Node) allowed(ip netip.Addr, op byte, proven bool, now time.Time) bool {
	per := allowedPerMinute(op)
	if per == 0 {
		return true
	}
	if op == OpFirewalled2Req {
		op = OpFirewalledReq // counted together
	}
	every := time.Minute / time.Duration(per)
	e := n.inTracked[ip]
	if e == nil {
		if len(n.inTracked) >= maxTrackedIPs {
			if now.Sub(n.inCleanup) >= time.Second {
				n.cleanFlood(now)
			}
			if len(n.inTracked) >= maxTrackedIPs {
				return false // no room to count it: refused
			}
		}
		e = &floodEntry{}
		n.inTracked[ip] = e
	}
	var fc *floodCount
	for i := range e.ops {
		if e.ops[i].op == op {
			fc = &e.ops[i]
			break
		}
	}
	if fc == nil {
		e.ops = append(e.ops, floodCount{op: op, first: now})
		fc = &e.ops[len(e.ops)-1]
	}
	if fc.count > 0 && now.Sub(fc.first) > every {
		gone := int(now.Sub(fc.first) / every)
		if gone > fc.count {
			fc.count, fc.first = 0, now
		} else {
			fc.count -= gone
			fc.first = fc.first.Add(every * time.Duration(gone))
		}
	}
	fc.count++
	if exp := fc.first.Add(every * time.Duration(fc.count)); exp.After(e.expires) {
		e.expires = exp
	}
	switch {
	case fc.count > per*5 && proven:
		if _, ok := n.banned[ip]; !ok && len(n.banned) < maxBanned {
			n.banned[ip] = now.Add(banTime)
			n.cfg.Log("kad: banned %s for an hour: flooding with opcode 0x%02X", ip, op)
		}
		return false
	case fc.count > per:
		return false
	}
	return true
}

// Challenges for nodes too old to prove their IP with verify keys: a
// KADEMLIA2_REQ with a random target (versions before 7) or a PING
// (version 7) whose answer must come back from that IP.
type challenge struct {
	ip        netip.Addr
	op        byte
	contact   ID
	challenge ID
	at        time.Time
}

func (n *Node) hasChallenge(ip netip.Addr, now time.Time) bool {
	for _, c := range n.challenges {
		if c.ip == ip && now.Sub(c.at) <= trackWindow {
			return true
		}
	}
	return false
}

func (n *Node) addChallenge(c challenge) {
	n.challenges = append(n.challenges, c)
	kept := n.challenges[:0]
	for _, o := range n.challenges {
		if c.at.Sub(o.at) <= trackWindow {
			kept = append(kept, o)
		}
	}
	n.challenges = kept
}

// takeChallenge reports whether an answer from ip to op meets a pending
// challenge (zero challenge ID: any answer), and returns the contact.
func (n *Node) takeChallenge(chal ID, ip netip.Addr, op byte, now time.Time) (ID, bool) {
	for i, c := range n.challenges {
		if c.ip == ip && c.op == op && now.Sub(c.at) < trackWindow && (c.challenge.IsZero() || c.challenge == chal) {
			n.challenges = append(n.challenges[:i], n.challenges[i+1:]...)
			return c.contact, true
		}
	}
	return ID{}, false
}

func (n *Node) sendLegacyChallenge(addr netip.AddrPort, contact ID, now time.Time) {
	if n.hasChallenge(addr.Addr(), now) {
		return
	}
	chal := RandomID()
	var w proto.Writer
	w.U8(findValue)
	WriteID(&w, chal)
	WriteID(&w, contact)
	n.send(addr, OpReq, w.B, ID{}, 0)
	n.addChallenge(challenge{ip: addr.Addr(), op: OpReq, contact: contact, challenge: chal, at: now})
}

// bootstrapAnswer is how many contacts we give a node that bootstraps
// from us (eMule gives 20).
const bootstrapAnswer = 20

// errBad marks a malformed or unrequested packet.
var errBad = errors.New("kad: bad packet")

// incoming is a decoded datagram.
type incoming struct {
	from      netip.AddrPort
	op        byte
	payload   []byte
	validKey  bool   // it carried the verify key we gave its IP
	senderKey UDPKey // the key it gives us
}

// handle processes one datagram.
func (n *Node) handle(data []byte, from netip.AddrPort, now time.Time) {
	ip := from.Addr()
	if !ip.Is4() || from.Port() == 0 {
		return
	}
	if n.isBanned(ip, now) {
		return
	}
	rcv, err := n.keys.Decrypt(data, ip)
	if err != nil {
		return
	}
	if len(rcv.Data) >= 2 && rcv.Data[0] == ProtoEMule {
		n.onClientUDP(rcv.Data, from, now)
		return
	}
	pkt, err := DecodePacket(rcv.Data)
	if err != nil {
		return
	}
	in := incoming{from: from, op: pkt.Opcode, payload: pkt.Payload, validKey: rcv.ValidKey}
	if rcv.SenderKey != 0 {
		in.senderKey = UDPKey{Key: rcv.SenderKey, IP: n.publicIP()}
	}
	// DNS port: only with obfuscation (eMule).
	if from.Port() == 53 && in.senderKey.Key == 0 {
		return
	}
	n.lastContact = now
	if !n.allowed(ip, in.op, in.validKey, now) {
		return
	}
	if err := n.dispatch(in, now); err != nil && n.cfg.Log != nil {
		_ = err // malformed packets are common on a public network: dropped quietly
	}
}

func (n *Node) dispatch(in incoming, now time.Time) error {
	switch in.op {
	case OpBootstrapReq:
		return n.onBootstrapReq(in)
	case OpBootstrapRes:
		return n.onBootstrapRes(in, now)
	case OpHelloReq:
		return n.onHelloReq(in, now)
	case OpHelloRes:
		return n.onHelloRes(in, now)
	case OpHelloResAck:
		return n.onHelloResAck(in, now)
	case OpReq:
		return n.onReq(in)
	case OpRes:
		return n.onRes(in, now)
	case OpSearchRes, OpSearchResV1:
		return n.onSearchRes(in, now)
	case OpPublishKeyReq:
		return n.onPublishKeyReq(in, now)
	case OpPublishSourceReq:
		return n.onPublishSourceReq(in, now)
	case OpPublishNotesReq:
		return n.onPublishNotesReq(in, now)
	case OpSearchKeyReq:
		return n.onSearchKeyReq(in, now)
	case OpSearchSourceReq:
		return n.onSearchSourceReq(in, now)
	case OpSearchNotesReq:
		return n.onSearchNotesReq(in, now)
	case OpPublishRes, OpPublishResV1:
		return n.onPublishRes(in, now)
	case OpFindBuddyReq:
		return n.onFindBuddyReq(in, now)
	case OpFindBuddyRes:
		return n.onFindBuddyRes(in, now)
	case OpCallbackReq:
		return n.onCallbackReq(in)
	case OpFirewallUDP:
		return n.onFirewallUDP(in, now)
	case OpPing:
		var w proto.Writer
		w.U16(in.from.Port())
		n.reply(in, OpPong, w.B)
		return nil
	case OpPong:
		return n.onPong(in, now)
	case OpFirewalledReq, OpFirewalled2Req:
		return n.onFirewalledReq(in)
	case OpFirewalledRes:
		return n.onFirewalledRes(in, now)
	case OpFirewalledAckRes:
		// Only nodes too old to answer over TCP send it, and only to a
		// request of the old kind.
		req := n.fwRequest(in.from.Addr(), now)
		if len(in.payload) != 0 || req == nil || req.op != OpFirewalledReq {
			return errBad
		}
		n.fwAck(in.from.Addr())
		return nil
	}
	return nil
}

// reply answers a request, obfuscated with the key the sender gave us.
func (n *Node) reply(in incoming, op byte, payload []byte) {
	n.send(in.from, op, payload, ID{}, in.senderKey.For(n.publicIP()))
}

func readContact(r *proto.Reader) Contact {
	c := Contact{ID: ReadID(r)}
	c.IP = ipFromU32(r.U32())
	c.UDPPort = r.U16()
	c.TCPPort = r.U16()
	c.Version = r.U8()
	return c
}

func writeContact(w *proto.Writer, c Contact) {
	WriteID(w, c.ID)
	w.U32(ipToU32(c.IP))
	w.U16(c.UDPPort)
	w.U16(c.TCPPort)
	w.U8(c.Version)
}

func (n *Node) onBootstrapReq(in incoming) error {
	// Verified contacts only: we do not pass on nodes nobody vouched for.
	var cs []Contact
	for _, c := range n.table.BootstrapContacts(200) {
		if c.Verified && len(cs) < bootstrapAnswer {
			cs = append(cs, c)
		}
	}
	var w proto.Writer
	WriteID(&w, n.keys.KadID)
	w.U16(n.cfg.TCPPort())
	w.U8(Version)
	w.U16(uint16(len(cs)))
	for _, c := range cs {
		writeContact(&w, c)
	}
	n.reply(in, OpBootstrapRes, w.B)
	return nil
}

func (n *Node) onBootstrapRes(in incoming, now time.Time) error {
	if !n.wasAsked(in.from.Addr(), OpBootstrapReq, now, false) {
		return errBad
	}
	r := proto.NewReader(in.payload)
	sender := Contact{ID: ReadID(r), IP: in.from.Addr(), UDPPort: in.from.Port(), Key: in.senderKey, Verified: in.validKey}
	sender.TCPPort = r.U16()
	sender.Version = r.U8()
	count := int(r.U16())
	if r.Err() != nil || r.Len() != count*25 {
		return errBad
	}
	if len(n.bootstrapList) == 0 {
		n.table.Add(sender, true, false, now)
	}
	// The contacts it names are not verified, even when they are all we
	// know: one node must not fill our table with its own. HELLOs verify
	// them (tick).
	for range count {
		c := readContact(r)
		c.Verified = false
		n.table.Add(c, false, false, now)
	}
	return nil
}

// helloInfo is what a HELLO carries.
type helloInfo struct {
	contact       Contact
	udpFirewalled bool
	tcpFirewalled bool
	wantsAck      bool
}

func readHello(in incoming) (helloInfo, error) {
	r := proto.NewReader(in.payload)
	h := helloInfo{contact: Contact{ID: ReadID(r), IP: in.from.Addr(), UDPPort: in.from.Port(), Key: in.senderKey, Verified: in.validKey}}
	h.contact.TCPPort = r.U16()
	h.contact.Version = r.U8()
	if r.Err() != nil || h.contact.Version == 0 {
		return h, errBad
	}
	tags, err := ReadTags(r)
	if err != nil {
		return h, errBad
	}
	if p, ok := TagInt(tags, TagSourceUDPPort); ok && uint16(p) > 0 {
		h.contact.UDPPort = uint16(p)
	}
	if o, ok := TagInt(tags, TagKadMiscOptions); ok {
		h.udpFirewalled = o&1 != 0
		h.tcpFirewalled = o&2 != 0
		h.wantsAck = o&4 != 0 && h.contact.Version >= 8
	}
	return h, nil
}

// addHello adds or refreshes the sender of a HELLO; nodes that are UDP
// firewalled cannot be reached, so they are not added.
func (n *Node) addHello(h helloInfo, fromReq bool, now time.Time) bool {
	if h.udpFirewalled {
		return false
	}
	ok, _ := n.table.Add(h.contact, true, fromReq, now)
	return ok
}

// sendHello sends our details: KADEMLIA2_HELLO_REQ or _RES.
func (n *Node) sendHello(op byte, addr netip.AddrPort, version byte, receiverKey uint32, target ID, requestAck bool) {
	if version < 2 {
		return
	}
	var w proto.Writer
	WriteID(&w, n.keys.KadID)
	w.U16(n.cfg.TCPPort())
	w.U8(Version)
	fw, udpFW := n.firewalled(), n.udpFirewalled()
	var tags []proto.Tag
	if n.udp.useIntern {
		tags = append(tags, IntTag(TagSourceUDPPort, uint64(n.localPort())))
	}
	if version >= 8 && (requestAck || fw || udpFW) {
		var o uint64
		if requestAck {
			o |= 4
		}
		if fw {
			o |= 2
		}
		if udpFW {
			o |= 1
		}
		tags = append(tags, IntTag(TagKadMiscOptions, o))
	}
	WriteTags(&w, tags)
	if version >= 6 {
		n.send(addr, op, w.B, target, receiverKey)
	} else {
		n.send(addr, op, w.B, ID{}, 0)
	}
}

func (n *Node) onHelloReq(in incoming, now time.Time) error {
	h, err := readHello(in)
	if err != nil || h.contact.Version < 2 {
		return errBad
	}
	added := n.addHello(h, true, now)
	addr := netip.AddrPortFrom(in.from.Addr(), h.contact.UDPPort)
	n.sendHello(OpHelloRes, addr, h.contact.Version, in.senderKey.For(n.publicIP()), h.contact.ID, added && !in.validKey)
	switch {
	case added && !in.validKey && h.contact.Version == 7 && !n.hasChallenge(addr.Addr(), now):
		// Version 7 cannot acknowledge a HELLO_RES: a PING answered
		// from that IP proves it.
		n.send(addr, OpPing, nil, ID{}, in.senderKey.For(n.publicIP()))
		n.addChallenge(challenge{ip: addr.Addr(), op: OpPing, contact: h.contact.ID, at: now})
	case n.findingExtPort() && h.contact.Version > 5:
		n.send(addr, OpPing, nil, ID{}, in.senderKey.For(n.publicIP()))
	}
	if added && !in.validKey && h.contact.Version < 7 {
		n.sendLegacyChallenge(addr, h.contact.ID, now)
	}
	n.maybeFirewallCheck(addr, h.contact, in, now)
	return nil
}

func (n *Node) onHelloRes(in incoming, now time.Time) error {
	if !n.wasAsked(in.from.Addr(), OpHelloReq, now, false) {
		return errBad
	}
	h, err := readHello(in)
	if err != nil {
		return errBad
	}
	added := n.addHello(h, true, now)
	addr := netip.AddrPortFrom(in.from.Addr(), h.contact.UDPPort)
	if h.wantsAck {
		if k := in.senderKey.For(n.publicIP()); k != 0 {
			var w proto.Writer
			WriteID(&w, n.keys.KadID)
			w.U8(0)
			n.send(addr, OpHelloResAck, w.B, ID{}, k)
		}
	} else if added && !in.validKey && h.contact.Version < 7 {
		n.sendLegacyChallenge(addr, h.contact.ID, now)
	}
	if n.findingExtPort() && h.contact.Version > 5 {
		n.send(addr, OpPing, nil, ID{}, in.senderKey.For(n.publicIP()))
	}
	n.maybeFirewallCheck(addr, h.contact, in, now)
	return nil
}

func (n *Node) onHelloResAck(in incoming, now time.Time) error {
	if len(in.payload) < 17 || !n.wasAsked(in.from.Addr(), OpHelloRes, now, false) {
		return errBad
	}
	if !in.validKey {
		return nil
	}
	id := ReadID(proto.NewReader(in.payload))
	n.table.Verify(id, in.from.Addr())
	return nil
}

func (n *Node) onReq(in incoming) error {
	r := proto.NewReader(in.payload)
	typ := r.U8() & 0x1F
	target := ReadID(r)
	check := ReadID(r)
	if r.Err() != nil || typ == 0 {
		return errBad
	}
	if check != n.keys.KadID {
		return nil // meant for another node
	}
	cs := n.table.Closest(target, 2, int(typ))
	var w proto.Writer
	WriteID(&w, target)
	w.U8(uint8(len(cs)))
	for _, c := range cs {
		writeContact(&w, c)
	}
	n.reply(in, OpRes, w.B)
	return nil
}

func (n *Node) onRes(in incoming, now time.Time) error {
	if !n.wasAsked(in.from.Addr(), OpReq, now, false) {
		return errBad
	}
	r := proto.NewReader(in.payload)
	target := ReadID(r)
	count := int(r.U8())
	if r.Err() != nil {
		return errBad
	}
	if id, ok := n.takeChallenge(target, in.from.Addr(), OpReq, now); ok {
		n.table.Verify(id, in.from.Addr())
		return nil
	}
	if r.Len() != count*25 {
		return errBad
	}
	s := n.searches[target]
	fwCheck := s != nil && s.typ == searchNodeFWCheck && n.udp.running
	var results []Contact
	for range count {
		c := readContact(r)
		if c.Version <= 1 || !GoodAddr(c.IP, c.UDPPort) || (c.UDPPort == 53 && c.Version <= 5) {
			continue
		}
		if fwCheck {
			// UDP test candidates: nodes we never talked to, kept out of
			// the table (and its hellos) until tested.
			if len(n.udp.candidates) < 100 && n.table.Get(c.ID) == nil {
				n.udp.candidates = append(n.udp.candidates, c)
			}
			continue
		}
		added, _ := n.table.AddUnfiltered(c, false, false, now)
		if added || n.table.Acceptable(c) {
			results = append(results, c)
		}
	}
	if s != nil {
		n.processResponse(s, in.from, results, now)
	}
	return nil
}

func (n *Node) onPong(in incoming, now time.Time) error {
	if len(in.payload) < 2 {
		return errBad
	}
	if p := n.pending[in.from.Addr()]; p != nil && (in.validKey || !p.needKey) {
		n.proved(in.from.Addr(), now) // publishes waiting for this answer
		return nil
	}
	if !n.wasAsked(in.from.Addr(), OpPing, now, false) {
		return errBad
	}
	if id, ok := n.takeChallenge(ID{}, in.from.Addr(), OpPing, now); ok {
		n.table.Verify(id, in.from.Addr())
		return nil
	}
	if n.findingExtPort() {
		n.setExtPort(proto.NewReader(in.payload).U16(), in.from.Addr())
	}
	return nil
}

// Our external UDP port: what 2 of 3 nodes saw us send from.
func (n *Node) findingExtPort() bool { return len(n.extPortIPs) < externalPortAskIPs }

func (n *Node) setExtPort(port uint16, from netip.Addr) {
	for _, ip := range n.extPortIPs {
		if ip == from {
			return
		}
	}
	n.extPortIPs = append(n.extPortIPs, from)
	for _, p := range n.extPorts {
		if p == port {
			n.extPort = port
			for len(n.extPortIPs) < externalPortAskIPs {
				n.extPortIPs = append(n.extPortIPs, netip.Addr{})
			}
			return
		}
	}
	n.extPorts = append(n.extPorts, port)
	if !n.findingExtPort() {
		n.extPort = 0 // three different answers: no idea
	}
}

// Firewall check: while fewer than 4 nodes answered, every node that says
// hello is asked to connect to our TCP port. 2 of them getting through
// means we are not firewalled.

func (n *Node) recheckFirewalled(now time.Time) {
	n.startUDPTest(now)
	n.lastFWState = n.firewalledN < 2
	n.firewalledN = 0
	n.recheckIP = 0
	clear(n.fwAsked)
	n.nextFWCheck = now.Add(time.Hour)
}

func (n *Node) firewalled() bool {
	if n.firewalledN < 2 {
		if n.recheckIP < firewallChecks {
			return n.lastFWState
		}
		return true
	}
	return false
}

// Firewalled reports whether other nodes cannot connect to our TCP port.
func (n *Node) Firewalled() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.firewalled()
}

func (n *Node) maybeFirewallCheck(addr netip.AddrPort, c Contact, in incoming, now time.Time) {
	port := n.cfg.TCPPort()
	if n.recheckIP >= firewallChecks || port == 0 {
		return
	}
	// Only nodes that proved their IP (they answered with our key): a
	// spoofed HELLO must not make us ask an address whose spoofed answers
	// would then decide our firewall state or our IP.
	if !in.validKey || c.Version < 6 {
		return
	}
	if _, ok := n.fwAsked[addr.Addr()]; ok || len(n.fwAsked) >= 3*firewallChecks {
		return
	}
	var w proto.Writer
	w.U16(port)
	op := byte(OpFirewalledReq)
	if c.Version > 6 {
		op = OpFirewalled2Req
		WriteID(&w, ID(n.keys.UserHash))
		w.U8(0) // connect options: no TCP obfuscation
	}
	n.send(addr, op, w.B, ID{}, in.senderKey.For(n.publicIP()))
	n.fwAsked[addr.Addr()] = &fwRequest{at: now, op: op}
}

// fwRequest is a firewall check we asked a node for.
type fwRequest struct {
	at       time.Time
	op       byte // OpFirewalledReq or OpFirewalled2Req
	answered bool // its FIREWALLED_RES came (counted once)
	acked    bool // it got through to our TCP port (counted once)
}

// fwRequest returns the pending request to ip, or nil.
func (n *Node) fwRequest(ip netip.Addr, now time.Time) *fwRequest {
	r := n.fwAsked[ip]
	if r == nil || now.Sub(r.at) > trackWindow {
		return nil
	}
	return r
}

func (n *Node) onFirewalledReq(in incoming) error {
	r := proto.NewReader(in.payload)
	port := r.U16()
	var user ID
	if in.op == OpFirewalled2Req {
		if len(in.payload) < 19 {
			return errBad
		}
		user = ReadID(r)
	} else if len(in.payload) != 2 {
		return errBad
	}
	if port == 0 || !GoodAddr(in.from.Addr(), port) || n.cfg.ConnectBack == nil {
		return nil
	}
	if in.from.Addr() == n.publicIP() && port == n.cfg.TCPPort() {
		return nil // ourselves
	}
	if !n.cfg.ConnectBack(in.from.Addr(), port, user) {
		return nil
	}
	var w proto.Writer
	w.U32(ipToU32(in.from.Addr()))
	n.reply(in, OpFirewalledRes, w.B)
	return nil
}

func (n *Node) onFirewalledRes(in incoming, now time.Time) error {
	if len(in.payload) != 4 {
		return errBad
	}
	req := n.fwRequest(in.from.Addr(), now)
	if req == nil || req.answered || !in.validKey {
		return errBad
	}
	req.answered = true
	ip := ipFromU32(proto.NewReader(in.payload).U32())
	if GoodAddr(ip, 1) {
		n.setExtIP(ip)
	}
	n.recheckIP++
	return nil
}

// setExtIP takes our IP from a node's answer; a change needs two nodes
// agreeing.
func (n *Node) setExtIP(ip netip.Addr) {
	if !n.extIPLast.IsValid() {
		n.extIP, n.extIPLast = ip, ip
		return
	}
	if ip == n.extIPLast {
		n.extIP = ip
	} else {
		n.extIPLast = ip
	}
}

// TCPCheckAck is called when a node connected to our TCP port to tell us
// it got through (OP_KAD_FWTCPCHECK_ACK).
func (n *Node) TCPCheckAck(ip netip.Addr) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.fwAck(ip.Unmap())
}

// fwAck counts a node that got through, once per node we asked.
func (n *Node) fwAck(ip netip.Addr) bool {
	req := n.fwRequest(ip, n.now())
	if req == nil || req.acked {
		return false
	}
	req.acked = true
	n.firewalledN++
	return true
}

// SendFirewalledAck tells a node of Kad version 6 or older that we got
// through to its TCP port (KADEMLIA_FIREWALLED_ACK_RES, to its Kad port).
func (n *Node) SendFirewalledAck(ip netip.Addr, kadPort uint16) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if GoodAddr(ip, kadPort) {
		n.send(netip.AddrPortFrom(ip.Unmap(), kadPort), OpFirewalledAckRes, nil, ID{}, 0)
	}
}

// ExternalAddr returns our IP and UDP port as Kad nodes see them (zero
// when unknown).
func (n *Node) ExternalAddr() (netip.Addr, uint16) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.extIP, n.extPort
}
