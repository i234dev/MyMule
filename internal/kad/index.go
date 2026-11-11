package kad

import (
	"bytes"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"mymule/internal/proto"
)

// What other nodes publish to us, as the node closest to a keyword or a
// file (eMule's CIndexed): keyword entries (files whose names have the
// keyword), sources of files, and notes (comments and ratings).

// Lifetimes and limits (eMule's, plus totals of our own so that nobody can
// fill our memory: eMule only limits each key, and counts entries, not
// bytes).
const (
	keywordLifetime = 24 * time.Hour
	sourceLifetime  = 5 * time.Hour
	notesLifetime   = 24 * time.Hour

	maxKeywordEntries  = 60000 // keyword entries in all
	maxFilesPerKeyword = 50000
	maxSourcesPerFile  = 1000
	maxNotesPerFile    = 150
	maxSourceEntries   = 60000 // ours
	maxNotesEntries    = 10000 // ours
	maxKeys            = 20000 // keys of each kind, ours
	maxSizesPerFile    = 4     // entries of one file under one keyword, ours

	// Keyword entries one address, and one /24, may have created: without
	// them a single node takes every place in a minute (a few publish
	// packets of 20,000 entries) and honest ones are told we are full.
	maxEntriesPerIP     = 1000
	maxEntriesPerSubnet = 2000
	// evictSample is how many entries are looked at to find one to drop
	// when the index is full.
	evictSample = 64

	maxNamesPerEntry      = 100
	maxPublishersPerEntry = 100
	maxTagsPerEntry       = 32
	maxNameBytes          = 255  // a file name, as eMule's limit
	maxEntryTagBytes      = 1024 // the tags of one entry, ours
	maxIndexBytes         = 64 << 20

	trustRefresh = 10 * time.Minute // eMule recomputes the trust this often
)

// entry is one published item.
type entry struct {
	key, source ID // keyword or file; file or client
	ip          netip.Addr
	udpPort     uint16
	tcpPort     uint16
	size        uint64
	tags        []proto.Tag // the tags kept and sent back
	expires     time.Time

	// Keyword entries: the names it was published under and who
	// published it.
	names      []fileName
	publishers []publisher

	// weak: published without our key (its address only answered a ping,
	// which any node does): stored, but it votes for nothing.
	weak bool

	// owner is the address that created a keyword entry (counted for the
	// limits per address and /24; refreshes by others do not change it).
	owner netip.Addr

	lname   string    // the name, lowercase (for searches)
	trustV  float64   // cached trust
	trustAt time.Time // when it was computed
	cost    int       // bytes counted for it
}

type fileName struct {
	name       string
	popularity int
}

type publisher struct {
	ip   netip.Addr
	last time.Time
	weak bool // not counted for trust (see entry.weak)
}

// name is the name most publishers used.
func (e *entry) name() string {
	best := -1
	out := ""
	for _, n := range e.names {
		if n.popularity > best {
			best, out = n.popularity, n.name
		}
	}
	return out
}

// Memory an entry takes, roughly (64-bit): the struct and what points to
// it, and each publisher, name and tag. The limits on the index's bytes
// count what it really holds with these.
const (
	entryCost     = 300
	publisherCost = 56
	nameCost      = 24
	tagCost       = 88
)

// settle recomputes what derives from the names and tags.
func (e *entry) settle() {
	e.lname = strings.ToLower(e.name())
	c := entryCost + len(e.lname) + publisherCost*len(e.publishers)
	for _, n := range e.names {
		c += nameCost + len(n.name)
	}
	for _, t := range e.tags {
		c += tagCost + len(t.Name) + len(t.Str) + len(t.Bytes)
	}
	e.cost = c
}

// cutName shortens a name to maxNameBytes, at a character boundary.
func cutName(s string) string {
	if len(s) <= maxNameBytes {
		return s
	}
	i := maxNameBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// index holds what was published to us.
type index struct {
	keywords map[ID]map[ID][]*entry // keyword -> file -> entries (one per size)
	sources  map[ID][]*entry        // file -> sources, newest first
	notes    map[ID][]*entry        // file -> notes, newest first

	nKeyword, nSource, nNotes int
	bytes                     int // what all entries cost

	// Publishers of keyword entries per /24, for the trust value.
	pubSubnets map[[3]byte]int

	// Keyword entries per owner address and /24.
	ownIPs     map[netip.Addr]int
	ownSubnets map[[3]byte]int

	// sorted holds the files of a keyword in the order searches take them,
	// until the keyword's files change.
	sorted map[ID][]ID

	// Our keywords whose nodes said they are overloaded: not published
	// again before the time given.
	load map[ID]time.Time

	nextClean time.Time
}

func newIndex() *index {
	return &index{
		keywords: map[ID]map[ID][]*entry{}, sources: map[ID][]*entry{}, notes: map[ID][]*entry{},
		pubSubnets: map[[3]byte]int{}, load: map[ID]time.Time{},
		ownIPs: map[netip.Addr]int{}, ownSubnets: map[[3]byte]int{},
		sorted: map[ID][]ID{},
	}
}

// own counts (delta 1) or uncounts (-1) a keyword entry for its owner.
func (x *index) own(ip netip.Addr, delta int) {
	if !ip.IsValid() {
		return
	}
	if n := x.ownIPs[ip] + delta; n > 0 {
		x.ownIPs[ip] = n
	} else {
		delete(x.ownIPs, ip)
	}
	s := subnet(ip)
	if n := x.ownSubnets[s] + delta; n > 0 {
		x.ownSubnets[s] = n
	} else {
		delete(x.ownSubnets, s)
	}
}

// ownerFull reports whether ip, or its /24, has as many keyword entries
// as it may.
func (x *index) ownerFull(ip netip.Addr) bool {
	return x.ownIPs[ip] >= maxEntriesPerIP || x.ownSubnets[subnet(ip)] >= maxEntriesPerSubnet
}

// keepTags normalises tags for storing: integers as their smallest type;
// at most maxTagsPerEntry tags of maxEntryTagBytes in all (the others are
// dropped), strings at most maxNameBytes.
func keepTags(tags []proto.Tag) []proto.Tag {
	out := make([]proto.Tag, 0, min(len(tags), maxTagsPerEntry))
	budget := maxEntryTagBytes
	for _, t := range tags {
		if len(out) == maxTagsPerEntry {
			break
		}
		if t.Type == proto.TagUint32 || t.Type == proto.TagUint64 {
			n := IntTag(t.ID, t.Int)
			n.Name = t.Name
			t = n
		}
		if t.Type == proto.TagString {
			t.Str = cutName(t.Str)
		}
		cost := 8 + len(t.Name) + len(t.Str) + len(t.Bytes)
		if cost > budget || len(t.Name) > 64 {
			continue
		}
		budget -= cost
		out = append(out, t)
	}
	return out
}

// room reports whether c more bytes fit.
func (x *index) room(c int) bool { return x.bytes+c <= maxIndexBytes }

// addKeyword stores a keyword entry; it returns whether it was taken and
// the load (0-100) to report.
func (x *index) addKeyword(e *entry, now time.Time) (bool, byte) {
	for i := range e.names {
		e.names[i].name = cutName(e.names[i].name)
	}
	if e.size == 0 || len(e.names) == 0 || e.name() == "" {
		return false, 0
	}
	files := x.keywords[e.key]
	if files == nil && len(x.keywords) >= maxKeys {
		return false, 100
	}
	var old *entry
	for _, o := range files[e.source] {
		if o.size == e.size {
			old = o
			break
		}
	}
	if old != nil && e.weak {
		for _, p := range old.publishers {
			if p.ip == e.ip && !p.weak {
				// A weak publish from the address of a proven publisher
				// may be forged: it keeps nothing alive.
				return true, 1
			}
		}
	}
	if old == nil {
		if len(files[e.source]) >= maxSizesPerFile || x.ownerFull(e.ip) {
			return false, 100
		}
		e.owner = e.ip
	} else {
		e.owner = old.owner
	}
	// Merging into old adds at most the new name and one publisher.
	e.settle()
	for {
		total := len(files)
		_, known := files[e.source]
		full := total >= maxFilesPerKeyword || (known && total > maxFilesPerKeyword-5000)
		if !full && (old != nil || x.nKeyword < maxKeywordEntries) && x.room(e.cost+publisherCost) {
			break
		}
		// Full. Refreshes of what is there go in while there are bytes for
		// them; a new entry may take the place of one nobody vouches for.
		if old != nil || !x.evictFor(e, files, full, now) {
			return false, 100
		}
		files = x.keywords[e.key] // what was dropped may have been in it
	}
	list := files[e.source]
	oldCost := 0
	if old != nil {
		oldCost = old.cost
		for i, o := range list {
			if o == old {
				list = append(list[:i:i], list[i+1:]...)
				break
			}
		}
	}
	total := len(files)
	x.merge(e, old, now)
	e.settle()
	if files == nil {
		files = map[ID][]*entry{}
		x.keywords[e.key] = files
	}
	x.bytes += e.cost - oldCost
	if old == nil {
		x.nKeyword++
		x.own(e.owner, 1)
	}
	if _, ok := files[e.source]; !ok {
		delete(x.sorted, e.key)
	}
	files[e.source] = append([]*entry{e}, list...)
	if total == 0 {
		return true, 1
	}
	return true, byte(total * 100 / maxFilesPerKeyword)
}

// evictFor drops a keyword entry nobody vouches for (trust under 1: its
// publishers are all weak, or share their networks with many others), to
// make room for e, a proven entry from a network that publishes little
// (so that it would be trusted). With inKeyword, the entry is taken from
// files (the keyword is full); else from anywhere. Only evictSample
// entries are looked at, in the maps' random order: entries of a flood
// are most of a full index, so one is soon found.
func (x *index) evictFor(e *entry, files map[ID][]*entry, inKeyword bool, now time.Time) bool {
	if e.weak || x.pubSubnets[subnet(e.ip)] >= 10 {
		return false
	}
	seen := 0
	try := func(files map[ID][]*entry) bool {
		for _, list := range files {
			for _, v := range list {
				if seen++; seen > evictSample {
					return false
				}
				if x.trust(v, now) < 1 {
					x.removeKeyword(v)
					return true
				}
			}
		}
		return false
	}
	if inKeyword {
		return try(files)
	}
	for _, files := range x.keywords {
		if try(files) {
			return true
		}
		if seen > evictSample {
			return false
		}
	}
	return false
}

// removeKeyword drops a keyword entry from the index.
func (x *index) removeKeyword(v *entry) {
	files := x.keywords[v.key]
	list := files[v.source]
	for i, o := range list {
		if o == v {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(files, v.source)
		delete(x.sorted, v.key)
	} else {
		files[v.source] = list
	}
	if len(files) == 0 {
		delete(x.keywords, v.key)
	}
	x.nKeyword--
	x.bytes -= v.cost
	for _, p := range v.publishers {
		x.dropPublisher(p)
	}
	x.own(v.owner, -1)
}

// merge carries the names and publishers of old (the same file, size and
// keyword, published before) into e, which has the one new name and its
// publisher's IP (eMule's MergeIPsAndFilenames). Unlike eMule, a
// publisher adds its name only the first time: one IP republishing with
// ever new names does not fill the list.
func (x *index) merge(e, old *entry, now time.Time) {
	refresh := false
	if old != nil {
		e.publishers = append([]publisher(nil), old.publishers...)
		fast := false
		for i, p := range e.publishers {
			if p.ip == e.ip {
				refresh = true
				fast = now.Sub(p.last) < sourceLifetime-time.Hour
				e.publishers = append(e.publishers[:i:i], e.publishers[i+1:]...)
				np := publisher{ip: e.ip, last: now, weak: p.weak && e.weak}
				if p.weak && !np.weak {
					x.pubSubnets[subnet(e.ip)]++ // now proven
				}
				e.publishers = append(e.publishers, np)
				break
			}
		}
		cur := fileName{}
		if len(e.names) > 0 {
			cur = e.names[0]
		}
		e.names = nil
		dup := false
		for _, n := range old.names {
			if n.name == "" {
				continue
			}
			if strings.EqualFold(n.name, cur.name) {
				dup = true
				if !fast && !e.weak {
					n.popularity++ // a refresh in time counts; a faster one does not
				}
			}
			e.names = addName(e.names, n)
		}
		if !dup && !refresh && !e.weak && cur.name != "" {
			e.names = addName(e.names, cur)
		}
		e.trustV, e.trustAt = old.trustV, old.trustAt
		if e.weak && hasProven(old) {
			// A weak refresh (the address only answered a ping) changes
			// neither the tags nor the life of what a proven node stored.
			e.tags, e.expires = old.tags, old.expires
		}
	}
	if !refresh {
		e.publishers = append(e.publishers, publisher{ip: e.ip, last: now, weak: e.weak})
		if !e.weak {
			x.pubSubnets[subnet(e.ip)]++
		}
		if len(e.publishers) > maxPublishersPerEntry {
			// The oldest weak one goes first; a weak one never pushes out
			// a proven one (it is dropped itself instead).
			drop := -1
			for i, p := range e.publishers {
				if p.weak {
					drop = i
					break
				}
			}
			if drop < 0 {
				drop = 0
			}
			x.dropPublisher(e.publishers[drop])
			e.publishers = append(e.publishers[:drop:drop], e.publishers[drop+1:]...)
		}
		e.trustAt = time.Time{} // recompute
	}
}

// hasProven reports whether a keyword entry has a proven publisher.
func hasProven(e *entry) bool {
	for _, p := range e.publishers {
		if !p.weak {
			return true
		}
	}
	return false
}

func addName(names []fileName, n fileName) []fileName {
	if len(names) < maxNamesPerEntry {
		return append(names, n)
	}
	weakest := 0
	for i, o := range names {
		if o.popularity < names[weakest].popularity {
			weakest = i
		}
	}
	if n.popularity > names[weakest].popularity {
		names[weakest] = n
	}
	return names
}

func (x *index) dropPublisher(p publisher) {
	if p.weak {
		return
	}
	s := subnet(p.ip)
	if x.pubSubnets[s] <= 1 {
		delete(x.pubSubnets, s)
	} else {
		x.pubSubnets[s]--
	}
}

// trust is how much an entry's publishers agree: 10 points per publisher,
// shared among the publishers of its /24. Under 1, the entry may be spam
// from a single network. Recomputed every 10 minutes, as eMule.
func (x *index) trust(e *entry, now time.Time) float64 {
	if !e.trustAt.IsZero() && now.Sub(e.trustAt) < trustRefresh {
		return e.trustV
	}
	t := 0.0
	for _, p := range e.publishers {
		if p.weak {
			continue
		}
		if n := x.pubSubnets[subnet(p.ip)]; n > 0 {
			t += 10.0 / float64(n)
		}
	}
	e.trustV, e.trustAt = t, now
	return t
}

// addSource stores a source. One per IP and file: the same client (or
// another from its address) replaces its older entry, so that one machine
// cannot take every place of a file. A weak source never replaces a
// proven one.
func (x *index) addSource(e *entry) (bool, byte) {
	if !e.ip.IsValid() || e.tcpPort == 0 || e.udpPort == 0 || len(e.tags) == 0 {
		return false, 0
	}
	if e.weak {
		for _, o := range x.sources[e.key] {
			if o.ip == e.ip && !o.weak {
				return true, 1
			}
		}
	}
	e.settle()
	return x.addTo(x.sources, &x.nSource, maxSourceEntries, maxSourcesPerFile, e,
		func(o *entry) bool { return o.ip == e.ip })
}

// addNote stores a note; one per client (IP or ID).
func (x *index) addNote(e *entry) (bool, byte) {
	for i := range e.names {
		e.names[i].name = cutName(e.names[i].name)
	}
	if !e.ip.IsValid() || (len(e.tags) == 0 && e.size == 0 && e.name() == "") {
		return false, 0
	}
	if e.weak {
		for _, o := range x.notes[e.key] {
			if (o.ip == e.ip || o.source == e.source) && !o.weak {
				return true, 1 // a weak note never replaces a proven one
			}
		}
	}
	e.settle()
	return x.addTo(x.notes, &x.nNotes, maxNotesEntries, maxNotesPerFile, e,
		func(o *entry) bool { return o.ip == e.ip || o.source == e.source })
}

func (x *index) addTo(m map[ID][]*entry, n *int, maxTotal, perKey int, e *entry, same func(*entry) bool) (bool, byte) {
	list, ok := m[e.key]
	if !e.weak && (*n >= maxTotal || (!ok && len(m) >= maxKeys)) {
		// Full: a proven entry takes the place of a weak one.
		if x.dropWeak(m, n, e.key) {
			list, ok = m[e.key]
		}
	}
	if !ok {
		if len(m) >= maxKeys || *n >= maxTotal || !x.room(e.cost) {
			return false, 100
		}
		m[e.key] = []*entry{e}
		*n++
		x.bytes += e.cost
		return true, 1
	}
	load := byte(min(100, len(list)*100/perKey))
	for i, o := range list {
		if same(o) {
			if !x.room(e.cost - o.cost) {
				return false, 100
			}
			x.bytes += e.cost - o.cost
			list = append(list[:i:i], list[i+1:]...)
			m[e.key] = append([]*entry{e}, list...)
			return true, load
		}
	}
	if len(list) >= perKey {
		// Full: the oldest makes room; for a weak entry, only a weak one.
		drop := len(list) - 1
		if e.weak {
			drop = -1
			for i := len(list) - 1; i >= 0; i-- {
				if list[i].weak {
					drop = i
					break
				}
			}
			if drop < 0 {
				return false, 100
			}
		}
		gone := list[drop]
		if !x.room(e.cost - gone.cost) {
			return false, 100
		}
		x.bytes += e.cost - gone.cost
		rest := append(list[:drop:drop], list[drop+1:]...)
		m[e.key] = append([]*entry{e}, rest...)
		return true, 100
	}
	if *n >= maxTotal || !x.room(e.cost) {
		return false, 100
	}
	m[e.key] = append([]*entry{e}, list...)
	*n++
	x.bytes += e.cost
	return true, load
}

// weakSample is how many keys dropWeak looks at: weak entries of a flood
// are most of a full map, so one is soon found in its random order.
const weakSample = 256

// dropWeak drops a weak entry from m (not one of keep), to make room for
// a proven one. It reports whether it found one.
func (x *index) dropWeak(m map[ID][]*entry, n *int, keep ID) bool {
	seen := 0
	for key, list := range m {
		if seen++; seen > weakSample {
			return false
		}
		if key == keep {
			continue
		}
		for i := len(list) - 1; i >= 0; i-- {
			if !list[i].weak {
				continue
			}
			x.bytes -= list[i].cost
			*n--
			if len(list) == 1 {
				delete(m, key)
			} else {
				m[key] = append(list[:i:i], list[i+1:]...)
			}
			return true
		}
	}
	return false
}

// clean drops what expired, every 30 minutes (expired entries are not
// sent meanwhile).
func (x *index) clean(now time.Time) {
	if now.Before(x.nextClean) {
		return
	}
	x.nextClean = now.Add(30 * time.Minute)
	for key, files := range x.keywords {
		for file, list := range files {
			kept := list[:0]
			for _, e := range list {
				if e.expires.Before(now) {
					x.nKeyword--
					x.bytes -= e.cost
					for _, p := range e.publishers {
						x.dropPublisher(p)
					}
					x.own(e.owner, -1)
					continue
				}
				// Publishers that stopped republishing.
				for len(e.publishers) > 0 && now.Sub(e.publishers[0].last) > keywordLifetime {
					x.dropPublisher(e.publishers[0])
					e.publishers = e.publishers[1:]
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				delete(files, file)
				delete(x.sorted, key)
			} else {
				files[file] = kept
			}
		}
		if len(files) == 0 {
			delete(x.keywords, key)
		}
	}
	cleanList := func(m map[ID][]*entry, n *int) {
		for key, list := range m {
			kept := list[:0]
			for _, e := range list {
				if e.expires.Before(now) {
					*n--
					x.bytes -= e.cost
					continue
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				delete(m, key)
			} else {
				m[key] = kept
			}
		}
	}
	cleanList(x.sources, &x.nSource)
	cleanList(x.notes, &x.nNotes)
	for k, t := range x.load {
		if t.Before(now) {
			delete(x.load, k)
		}
	}
}

// searchWork is how much work one keyword search may take: about one unit
// per entry looked at, plus one per term of the search tree and per 64
// bytes of name it is checked against. It stops a search with a large
// tree over a large keyword (up to 50,000 entries, 64 terms) from holding
// the node's lock for a second; ordinary searches look at every entry.
const searchWork = 200000

// keywordResults returns the entries of a keyword that fit the tree,
// trusted ones first, skipping start of them, at most max. Entries are
// taken in a fixed order (by file, then size), so pages do not overlap.
func (x *index) keywordResults(key ID, tree *term, start, max int, now time.Time) []*entry {
	res, _ := x.keywordResultsWork(key, tree, start, max, now)
	return res
}

// keywordResultsWork is keywordResults, also returning the work it took.
func (x *index) keywordResultsWork(key ID, tree *term, start, max int, now time.Time) ([]*entry, int) {
	files := x.keywords[key]
	ids, ok := x.sorted[key]
	if !ok || len(ids) != len(files) {
		ids = make([]ID, 0, len(files))
		for id := range files {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
		if len(files) > 0 {
			x.sorted[key] = ids
		}
	}
	work := 0
	want := start + max
	var trusted, other []*entry
	for _, id := range ids {
		if work >= searchWork {
			break
		}
		for _, e := range files[id] {
			work++
			if e.expires.Before(now) {
				continue
			}
			trustedOne := x.trust(e, now) >= 1
			if !trustedOne && len(other) >= want {
				continue // enough of those already
			}
			if tree != nil && !tree.matchesWork(e, &work) {
				continue
			}
			if trustedOne {
				trusted = append(trusted, e)
				if len(trusted) >= want {
					break
				}
			} else {
				other = append(other, e)
			}
		}
		if len(trusted) >= want {
			break
		}
	}
	all := append(trusted, other...)
	if start >= len(all) {
		return nil, work
	}
	all = all[start:]
	return all[:min(len(all), max)], work
}

// listResults returns the sources or notes of a file of the given size (0
// = any), skipping start of them, at most max.
func listResults(list []*entry, size uint64, start, max int, now time.Time) []*entry {
	var out []*entry
	for _, e := range list {
		if e.expires.Before(now) {
			continue
		}
		if size != 0 && e.size != 0 && e.size != size {
			continue
		}
		if start > 0 {
			start--
			continue
		}
		if len(out) == max {
			break
		}
		out = append(out, e)
	}
	return out
}
