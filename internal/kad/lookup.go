package kad

import (
	"net/netip"
	"sort"
	"time"

	"mymule/internal/proto"
)

// Iterative lookups (eMule's CSearch): ask the nodes closest to a target
// for nodes even closer, until no closer ones turn up.

const (
	alphaQuery      = 5  // nodes asked at once
	findValue       = 2  // nodes asked for in a value search
	findValueMore   = 4  // ... when asking again for more
	store           = 4  // ... in a store
	findNode        = 11 // ... in a node lookup
	searchTolerance = 16777216
)

type searchType int

const (
	searchNode         searchType = iota // fill the table: stops at the first answer
	searchNodeComplete                   // our own ID: a full lookup
)

// lifetime is how long a search of each type runs.
func (t searchType) lifetime() time.Duration {
	return 45 * time.Second
}

// requestCount is how many nodes each KADEMLIA2_REQ asks for.
func (t searchType) requestCount() byte {
	switch t {
	case searchNode, searchNodeComplete:
		return findNode
	}
	return findValue
}

type search struct {
	typ          searchType
	target       ID
	created      time.Time
	lastResponse time.Time
	stopping     bool
	answers      int

	// Contacts by their distance to the target.
	possible      map[ID]Contact
	tried         map[ID]Contact
	responded     map[ID]bool // whether it gave closer contacts
	best          map[ID]Contact
	requestedMore map[ID]bool // by node ID
}

func newSearch(typ searchType, target ID, now time.Time) *search {
	return &search{
		typ: typ, target: target, created: now, lastResponse: now,
		possible: map[ID]Contact{}, tried: map[ID]Contact{}, responded: map[ID]bool{},
		best: map[ID]Contact{}, requestedMore: map[ID]bool{},
	}
}

func sortedKeys(m map[ID]Contact) []ID {
	keys := make([]ID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Cmp(keys[j]) < 0 })
	return keys
}

// prepareToStop lets the search run 15 more seconds for late answers.
func (s *search) prepareToStop(now time.Time) {
	if s.stopping {
		return
	}
	s.created = now.Add(-s.typ.lifetime() + 15*time.Second)
	s.stopping = true
}

// startSearch starts a lookup unless one for the same target runs.
func (n *Node) startSearch(typ searchType, target ID, now time.Time) *search {
	if _, ok := n.searches[target]; ok {
		return nil
	}
	s := newSearch(typ, target, now)
	n.searches[target] = s
	for _, c := range n.table.Closest(target, 3, 50) {
		s.possible[c.ID.Xor(target)] = c
	}
	count := min(alphaQuery, len(s.possible))
	if typ == searchNode {
		count = min(1, count)
	}
	for _, d := range sortedKeys(s.possible)[:count] {
		c := s.possible[d]
		s.tried[d] = c
		n.sendFindValue(s, c, false)
	}
	return s
}

func (n *Node) sendFindValue(s *search, c Contact, more bool) {
	if s.stopping {
		return
	}
	count := s.typ.requestCount()
	if more {
		s.requestedMore[c.ID] = true
		count = findValueMore
	}
	var w proto.Writer
	w.U8(count)
	WriteID(&w, s.target)
	WriteID(&w, c.ID)
	n.sendTo(c, OpReq, w.B)
}

// jumpStart moves a search on when nothing came for 3 seconds: ask the
// next closest node not asked yet.
func (n *Node) jumpStart(s *search, now time.Time) {
	if s.lastResponse.Add(3 * time.Second).After(now) {
		return
	}
	if len(s.possible) == 0 {
		s.prepareToStop(now)
		return
	}
	if len(s.requestedMore) == 0 && s.typ.requestCount() == findValue && len(s.tried) >= 3*findValue {
		tried := sortedKeys(s.tried)
		closest := true
		for _, d := range tried[:findValue] {
			if _, ok := s.responded[d]; ok {
				closest = false
				break
			}
		}
		if closest {
			// The closest ones are not answering: ask the closest that
			// did for more nodes.
			for _, d := range tried[findValue:] {
				if _, ok := s.responded[d]; ok {
					n.sendFindValue(s, s.tried[d], true)
					return
				}
			}
		}
	}
	for _, d := range sortedKeys(s.possible) {
		c := s.possible[d]
		if _, ok := s.tried[d]; ok {
			if _, ok := s.responded[d]; ok {
				n.storePacket(s, d, c, now)
			}
			delete(s.possible, d)
			continue
		}
		s.tried[d] = c
		n.sendFindValue(s, c, false)
		break
	}
}

// storePacket is where value searches ask a close node for the value (or
// store it); node lookups have nothing to do there.
func (n *Node) storePacket(s *search, dist ID, c Contact, now time.Time) {}

// processResponse handles the contacts a node sent back for a search.
func (n *Node) processResponse(s *search, from netip.AddrPort, results []Contact, now time.Time) {
	s.lastResponse = now
	var fromDist ID
	var fromContact *Contact
	for d, c := range s.tried {
		if c.IP == from.Addr() && c.UDPPort == from.Port() {
			fromDist, fromContact = d, &c
			break
		}
	}
	reasked := fromContact != nil && s.requestedMore[fromContact.ID]
	if len(results) > int(s.typ.requestCount()) && !(reasked && len(results) <= findValueMore) {
		return
	}
	if s.typ == searchNode {
		s.answers++
		clear(s.possible)
		return
	}
	if fromContact == nil {
		return
	}
	closer := false
	ips := map[netip.Addr]bool{from.Addr(): true}
	subnets := map[[3]byte]int{subnet(from.Addr()): 1}
	for _, c := range results {
		d := c.ID.Xor(s.target)
		if d.Cmp(fromDist) < 0 {
			closer = true
		}
		if _, ok := s.possible[d]; ok {
			continue
		}
		if _, ok := s.tried[d]; ok {
			continue
		}
		if ips[c.IP] {
			continue
		}
		ips[c.IP] = true
		if subnets[subnet(c.IP)] >= 2 {
			continue
		}
		subnets[subnet(c.IP)]++
		s.possible[d] = c
		if d.Cmp(fromDist) < 0 {
			top := false
			if len(s.best) < alphaQuery {
				top = true
				s.best[d] = c
			} else {
				keys := sortedKeys(s.best)
				if worst := keys[len(keys)-1]; d.Cmp(worst) < 0 {
					delete(s.best, worst)
					s.best[d] = c
					top = true
				}
			}
			if top {
				s.tried[d] = c
				n.sendFindValue(s, c, false)
			}
		}
	}
	s.responded[fromDist] = closer
	if s.typ == searchNodeComplete {
		s.answers++
	}
}

// tickSearches runs every second: ends searches past their lifetime and
// moves the others on.
func (n *Node) tickSearches(now time.Time) {
	for target, s := range n.searches {
		age := now.Sub(s.created)
		switch {
		case age > s.typ.lifetime():
			delete(n.searches, target)
			n.searchEnded(s)
		case s.typ == searchNodeComplete && age > 10*time.Second && s.answers > 10:
			delete(n.searches, target)
			n.searchEnded(s)
		default:
			n.jumpStart(s, now)
		}
	}
}

// searchEnded is called when a search is removed.
func (n *Node) searchEnded(s *search) {}
