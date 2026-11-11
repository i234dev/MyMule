package kad

import (
	"net/netip"
	"time"

	"mymule/internal/proto"
)

// The full-node side of Kad: nodes publish to us the keywords, sources
// and notes whose IDs are close to ours, and search them here.

// closeEnough reports whether target is within the part of the ID space
// we answer for (eMule's SEARCHTOLERANCE).
func (n *Node) closeEnough(target ID) bool {
	return n.keys.KadID.Xor(target).Chunk(0) <= searchTolerance
}

// storing reports whether we take publishes: only once we know other
// nodes can reach us over UDP (else nobody could search what we store,
// and the publisher would count us among the nodes that have it).
func (n *Node) storing() bool { return n.udpReachable() }

// Publishes that do not carry our key may come from a forged address: a
// forged source would make every downloader connect to someone else, and
// forged keyword publishes would vote for names and trust. Many honest
// nodes publish without our key (they know us from another node's
// answer), so such publishes wait: we ping their address, and keep them
// once it answers from there.
//
// What waits is limited for each address (one full publish packet) and in
// all, in entries and in bytes: forged addresses cost an attacker nothing,
// so only a total keeps them from filling our memory. When it is full,
// what waited longest makes room (a flood must then outpace the pings'
// answers, which take a fraction of a second, to push honest publishes
// out); proven ones (with our key) are stored at once and never wait.
const (
	maxPendingPerIP = filesPerPublish
	maxPendingIPs   = 2000
	maxPendingItems = 5000
	maxPendingBytes = 4 << 20
	// pendingWait is how long a publish waits for the ping's answer, which
	// comes within seconds.
	pendingWait = 30 * time.Second
)

type pendingKind int

const (
	pendKeyword pendingKind = iota
	pendSource
	pendNote
)

type pendingPub struct {
	at    time.Time
	items []pendingItem
	bytes int // what its items cost
	// needKey: our ping went obfuscated with the key the publisher gave
	// (so it carried ours): only an answer with our key counts, which a
	// forger cannot make.
	needKey bool
}

type pendingItem struct {
	kind pendingKind
	e    *entry
}

// add stores a publish now if it is proven, or else once its address
// answers our ping. It returns whether it was taken (or is waiting) and
// the load to report.
func (n *Node) add(in incoming, kind pendingKind, e *entry, now time.Time) (bool, byte) {
	if in.validKey {
		return n.addNow(kind, e, now)
	}
	// Without the sender's key our ping would go out in the clear, and any
	// PONG from that address, forged as easily as the publish, would
	// prove it. Only clients older than eMule 0.49a send no key: theirs
	// are dropped.
	if in.senderKey.For(n.publicIP()) == 0 {
		return false, 0
	}
	ip := in.from.Addr()
	e.settle()
	full := func() bool { return n.pendingItems >= maxPendingItems || n.pendingBytes+e.cost > maxPendingBytes }
	if full() {
		n.prunePending(now)
		for range 8 {
			if !full() || len(n.pending) == 0 {
				break
			}
			n.dropOldestPending()
		}
		if full() {
			return false, 0
		}
	}
	p := n.pending[ip]
	if p != nil && now.Sub(p.at) > pendingWait {
		n.dropPending(ip)
		p = nil
	}
	if p == nil {
		if len(n.pending) >= maxPendingIPs {
			n.prunePending(now)
			if len(n.pending) >= maxPendingIPs {
				n.dropOldestPending() // a flood of them must not lock others out
			}
		}
		p = &pendingPub{at: now, needKey: in.senderKey.For(n.publicIP()) != 0}
		n.pending[ip] = p
		// Not among the tracked requests: a flood of such publishes must
		// not push out those of our own lookups. The pending entry is what
		// lets the answer in.
		n.sendUntracked(in.from, OpPing, nil, ID{}, in.senderKey.For(n.publicIP()))
	}
	if len(p.items) >= maxPendingPerIP {
		return false, 0
	}
	p.items = append(p.items, pendingItem{kind, e})
	p.bytes += e.cost
	n.pendingItems++
	n.pendingBytes += e.cost
	return true, 1
}

// dropPending forgets what ip published and waits.
func (n *Node) dropPending(ip netip.Addr) *pendingPub {
	p := n.pending[ip]
	if p == nil {
		return nil
	}
	delete(n.pending, ip)
	n.pendingItems -= len(p.items)
	n.pendingBytes -= p.bytes
	return p
}

func (n *Node) dropOldestPending() {
	var oldest netip.Addr
	var at time.Time
	for ip, p := range n.pending {
		if !oldest.IsValid() || p.at.Before(at) {
			oldest, at = ip, p.at
		}
	}
	n.dropPending(oldest)
}

func (n *Node) addNow(kind pendingKind, e *entry, now time.Time) (bool, byte) {
	switch kind {
	case pendKeyword:
		return n.index.addKeyword(e, now)
	case pendSource:
		return n.index.addSource(e)
	default:
		return n.index.addNote(e)
	}
}

// proved stores what ip published, now that it answered our ping.
func (n *Node) proved(ip netip.Addr, now time.Time) {
	p := n.dropPending(ip)
	if p == nil {
		return
	}
	if now.Sub(p.at) > pendingWait {
		return
	}
	for _, it := range p.items {
		it.e.weak = true // the ping proves the address is live, not who sent
		n.addNow(it.kind, it.e, now)
	}
}

func (n *Node) prunePending(now time.Time) {
	for ip, p := range n.pending {
		if now.Sub(p.at) > pendingWait {
			n.dropPending(ip)
		}
	}
}

// readEntryTags reads a publish's tags into e: the name and size are kept
// apart; set picks out the tags that need more than storing.
func readEntryTags(r *proto.Reader, e *entry, set func(t proto.Tag) (keep bool)) error {
	tags, err := ReadTags(r)
	if err != nil {
		return err
	}
	var kept []proto.Tag
	for _, t := range tags {
		if t.Name == "" {
			switch t.ID {
			case TagFileName:
				if len(e.names) == 0 && t.Type == proto.TagString && t.Str != "" {
					e.names = []fileName{{name: cutName(t.Str), popularity: 1}}
				}
				continue
			case TagFileSize:
				if e.size == 0 {
					if t.Type == proto.TagBsob && len(t.Bytes) == 8 {
						e.size = proto.NewReader(t.Bytes).U64()
					} else {
						e.size = t.Int
					}
				}
				continue
			}
		}
		if set == nil || set(t) {
			kept = append(kept, t)
		}
	}
	e.tags = keepTags(kept)
	return nil
}

func (n *Node) onPublishKeyReq(in incoming, now time.Time) error {
	if !n.storing() || !GoodAddr(in.from.Addr(), in.from.Port()) {
		return nil // nor from addresses others could not reach
	}
	r := proto.NewReader(in.payload)
	key := ReadID(r)
	count := int(r.U16())
	if r.Err() != nil || count > r.Len()/(IDLen+1) {
		return errBad
	}
	if !n.closeEnough(key) {
		return nil
	}
	// The load reported is that of the entries taken: a few refused (say,
	// over a limit for one publisher) must not tell an honest publisher
	// that the keyword is overloaded.
	var load, refusedLoad byte
	taken := false
	for range count {
		e := &entry{key: key, source: ReadID(r), ip: in.from.Addr(), udpPort: in.from.Port(), expires: now.Add(keywordLifetime)}
		if err := readEntryTags(r, e, nil); err != nil {
			return errBad
		}
		if ok, l := n.add(in, pendKeyword, e, now); ok {
			taken, load = true, max(load, l)
		} else {
			refusedLoad = max(refusedLoad, l)
		}
	}
	if !taken {
		load = refusedLoad
	}
	var w proto.Writer
	WriteID(&w, key)
	w.U8(load)
	n.reply(in, OpPublishRes, w.B)
	return nil
}

func (n *Node) onPublishSourceReq(in incoming, now time.Time) error {
	if !n.storing() || !GoodAddr(in.from.Addr(), in.from.Port()) {
		return nil // nor from addresses others could not reach
	}
	r := proto.NewReader(in.payload)
	file := ReadID(r)
	source := ReadID(r)
	if r.Err() != nil {
		return errBad
	}
	if !n.closeEnough(file) {
		return nil
	}
	e := &entry{key: file, source: source, ip: in.from.Addr(), udpPort: in.from.Port(), expires: now.Add(sourceLifetime)}
	isSource, udpTag := false, false
	var extra []proto.Tag
	err := readEntryTags(r, e, func(t proto.Tag) bool {
		if t.Name != "" {
			return true
		}
		switch t.ID {
		case TagSourceType:
			if isSource {
				return false
			}
			isSource = true
			// The address is the one it sent from, not one it claims.
			extra = append(extra, IntTag(TagSourceIP, uint64(ipToU32(e.ip))))
			return true
		case TagSourceIP:
			return false
		case TagSourcePort:
			if e.tcpPort != 0 {
				return false
			}
			e.tcpPort = uint16(t.Int)
			return true
		case TagSourceUDPPort:
			if udpTag || t.Int == 0 {
				return false
			}
			e.udpPort, udpTag = uint16(t.Int), true
			return true
		}
		return true
	})
	if err != nil {
		return errBad
	}
	if !isSource {
		return nil
	}
	e.tags = keepTags(append(extra, e.tags...))
	if !udpTag {
		e.tags = append(e.tags, IntTag(TagSourceUDPPort, uint64(e.udpPort)))
	}
	ok, load := n.add(in, pendSource, e, now)
	if !ok {
		return nil
	}
	var w proto.Writer
	WriteID(&w, file)
	w.U8(load)
	n.reply(in, OpPublishRes, w.B)
	return nil
}

func (n *Node) onPublishNotesReq(in incoming, now time.Time) error {
	if !n.storing() || !GoodAddr(in.from.Addr(), in.from.Port()) {
		return nil // nor from addresses others could not reach
	}
	r := proto.NewReader(in.payload)
	file := ReadID(r)
	source := ReadID(r)
	if r.Err() != nil {
		return errBad
	}
	if !n.closeEnough(file) {
		return nil
	}
	e := &entry{key: file, source: source, ip: in.from.Addr(), udpPort: in.from.Port(), expires: now.Add(notesLifetime)}
	if err := readEntryTags(r, e, nil); err != nil {
		return errBad
	}
	ok, load := n.add(in, pendNote, e, now)
	if !ok {
		return nil
	}
	var w proto.Writer
	WriteID(&w, file)
	w.U8(load)
	n.reply(in, OpPublishRes, w.B)
	return nil
}

// Search answers: at most 50 results and maxAnswerBytes per packet, 300
// results in all. To a node that did not prove its address (our key), one
// small packet only: a small forged request must not make us send a flood
// to someone else.
const (
	resultsPerPacket  = 50
	maxResults        = 300
	maxNotesResults   = 150
	maxAnswerBytes    = 8 << 10
	maxUnprovenAnswer = 1200
)

// sendResults sends entries in KADEMLIA2_SEARCH_RES packets.
func (n *Node) sendResults(in incoming, target ID, entries []*entry, withPublishInfo bool) {
	packetBytes, packets := maxAnswerBytes, len(entries)
	if !in.validKey {
		packetBytes, packets = maxUnprovenAnswer, 1
	}
	now := n.now()
	for len(entries) > 0 && packets > 0 {
		packets--
		var body proto.Writer
		count := 0
		for len(entries) > 0 && count < resultsPerPacket {
			e := entries[0]
			var one proto.Writer
			WriteID(&one, e.source)
			tags := make([]proto.Tag, 0, len(e.tags)+3)
			if name := e.name(); name != "" {
				tags = append(tags, StrTag(TagFileName, name))
			}
			if e.size != 0 {
				tags = append(tags, IntTag(TagFileSize, e.size))
			}
			tags = append(tags, e.tags...)
			if withPublishInfo && len(e.publishers) > 0 {
				trust := min(uint64(n.index.trust(e, now)*100), 0xFFFF)
				info := uint64(len(e.names)&0xFF)<<24 | uint64(len(e.publishers)&0xFF)<<16 | trust
				tags = append(tags, IntTag(TagPublishInfo, info))
			}
			WriteTags(&one, tags)
			if 2*IDLen+2+len(body.B)+len(one.B) > packetBytes {
				if count == 0 {
					entries = entries[1:] // does not fit even alone
					continue
				}
				break
			}
			body.Bytes(one.B)
			count++
			entries = entries[1:]
		}
		if count == 0 {
			break
		}
		var w proto.Writer
		WriteID(&w, n.keys.KadID)
		WriteID(&w, target)
		w.U16(uint16(count))
		w.Bytes(body.B)
		n.reply(in, OpSearchRes, w.B)
	}
}

// searchWorkPerSecond is the work (see searchWork) keyword searches
// without our key may take per second in all: the limit per address does
// nothing against forged addresses. Ordinary searches cost little of it;
// searches with our key come from real addresses and are not counted.
const searchWorkPerSecond = 10 * searchWork

func (n *Node) onSearchKeyReq(in incoming, now time.Time) error {
	if sec := now.Truncate(time.Second); !sec.Equal(n.kwSecond) {
		n.kwSecond, n.kwWork = sec, 0
	}
	if !in.validKey && n.kwWork >= searchWorkPerSecond {
		return nil
	}
	r := proto.NewReader(in.payload)
	target := ReadID(r)
	start := r.U16()
	if r.Err() != nil {
		return errBad
	}
	var tree *term
	if start&0x8000 != 0 {
		t, err := parseTree(r, 0)
		if err != nil {
			return errBad
		}
		tree = t
	}
	n.index.clean(now)
	res, work := n.index.keywordResultsWork(target, tree, int(start&0x7FFF), maxResults, now)
	if !in.validKey {
		n.kwWork += work
	}
	n.sendResults(in, target, res, true)
	return nil
}

func (n *Node) onSearchSourceReq(in incoming, now time.Time) error {
	r := proto.NewReader(in.payload)
	target := ReadID(r)
	start := int(r.U16() & 0x7FFF)
	size := r.U64()
	if r.Err() != nil {
		return errBad
	}
	n.index.clean(now)
	// Sources published without our key (weak) after the proven ones:
	// their address only answered a ping, and the port is theirs to say.
	list := n.index.sources[target]
	ordered := make([]*entry, 0, len(list))
	for _, e := range list {
		if !e.weak {
			ordered = append(ordered, e)
		}
	}
	for _, e := range list {
		if e.weak {
			ordered = append(ordered, e)
		}
	}
	res := listResults(ordered, size, start, maxResults, now)
	n.sendResults(in, target, res, false)
	return nil
}

func (n *Node) onSearchNotesReq(in incoming, now time.Time) error {
	r := proto.NewReader(in.payload)
	target := ReadID(r)
	size := r.U64()
	if r.Err() != nil {
		return errBad
	}
	n.index.clean(now)
	res := listResults(n.index.notes[target], size, 0, maxNotesResults, now)
	n.sendResults(in, target, res, false)
	return nil
}
