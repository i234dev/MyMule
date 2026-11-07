package kad

import (
	"math/rand/v2"
	"net/netip"
	"sort"
	"time"
)

// Routing table constants (eMule's).
const (
	K     = 10 // contacts per bin
	kBase = 4  // zones up to this depth always split
	kK    = 5  // the first kK zones of each level split too

	maxContactsIP     = 1  // contacts per IP in the whole table
	maxContactsSubnet = 10 // contacts per /24 in the whole table
	maxBinSubnet      = 2  // contacts per /24 in one bin
)

// UDPKey is the verify key a node gave us, valid while our public IP is
// the one it was given to.
type UDPKey struct {
	Key uint32
	IP  netip.Addr // our public IP when the node gave us the key
}

// For returns the key if our public IP is still the one it was given to
// (both may be unknown, as in eMule), else 0.
func (k UDPKey) For(myIP netip.Addr) uint32 {
	if k.IP == myIP {
		return k.Key
	}
	return 0
}

// Contact is a Kad node.
type Contact struct {
	ID       ID
	IP       netip.Addr
	UDPPort  uint16
	TCPPort  uint16
	Version  byte
	Key      UDPKey
	Verified bool // its IP was proven (it answered with our verify key)

	// Liveness, as in eMule: type 0 (known for hours) to 3 (new); 4 =
	// being checked, removed when the check expires.
	typ           int
	created       time.Time
	expires       time.Time
	lastTypeSet   time.Time
	helloReceived bool
}

// Type is the contact's liveness class: 0 (best) to 4 (being dropped).
func (c *Contact) Type() int { return c.typ }

// Addr returns the contact's UDP address.
func (c *Contact) Addr() netip.AddrPort { return netip.AddrPortFrom(c.IP, c.UDPPort) }

func newContact(c Contact, now time.Time) *Contact {
	n := c
	n.typ = 3
	n.created = now
	n.lastTypeSet = now
	n.expires = time.Time{}
	return &n
}

// updateType classes a contact that just showed it is alive by how long
// we have known it.
func (c *Contact) updateType(now time.Time) {
	switch h := now.Sub(c.created) / time.Hour; {
	case h <= 0:
		c.typ, c.expires = 2, now.Add(time.Hour)
	case h == 1:
		c.typ, c.expires = 1, now.Add(90*time.Minute)
	default:
		c.typ, c.expires = 0, now.Add(2*time.Hour)
	}
}

// checkingType marks a contact as being checked: one step closer to 4.
func (c *Contact) checkingType(now time.Time) {
	if now.Sub(c.lastTypeSet) < 10*time.Second || c.typ == 4 {
		return
	}
	c.lastTypeSet = now
	c.expires = now.Add(2 * time.Minute)
	c.typ++
}

// GoodAddr reports whether ip:port may be a Kad node on the Internet: not
// private, loopback, multicast or reserved, and not port 0.
func GoodAddr(ip netip.Addr, port uint16) bool {
	ip = ip.Unmap()
	if !ip.Is4() || port == 0 || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	a := ip.As4()
	switch {
	case a[0] == 0, a[0] >= 224: // "this network", multicast, reserved, broadcast
		return false
	case a[0] == 100 && a[1]&0xC0 == 64: // carrier-grade NAT
		return false
	case a[0] == 198 && a[1]&0xFE == 18: // benchmarking
		return false
	case a[0] == 192 && a[1] == 0 && (a[2] == 0 || a[2] == 2), // protocol assignments, documentation
		a[0] == 198 && a[1] == 51 && a[2] == 100,
		a[0] == 203 && a[1] == 0 && a[2] == 113:
		return false
	}
	return true
}

func subnet(ip netip.Addr) [3]byte {
	a := ip.Unmap().As4()
	return [3]byte{a[0], a[1], a[2]}
}

// zone is a node of the routing tree: a leaf holding a bin of at most K
// contacts, or two sub-zones. Zones are in the space of distances to our
// ID: a zone at level L holds contacts whose distance starts with the L
// bits of its prefix.
type zone struct {
	parent    *zone
	level     int
	prefix    ID // the first level bits are the zone's path
	sub       [2]*zone
	bin       []*Contact // leaf only; oldest first
	nextSmall time.Time
	nextBig   time.Time
}

func (z *zone) leaf() bool { return z.sub[0] == nil }

// indexBelow reports whether the zone's index (its path read as a number)
// is below n (n <= 8).
func (z *zone) indexBelow(n int) bool {
	v := 0
	for i := 0; i < z.level; i++ {
		if i < z.level-3 {
			if z.prefix.Bit(i) != 0 {
				return false
			}
			continue
		}
		v = v<<1 | z.prefix.Bit(i)
	}
	return v < n
}

func (z *zone) canSplit() bool {
	return z.level < 127 && (z.indexBelow(kK) || z.level < kBase) && len(z.bin) >= K
}

// Table is the routing table. It is not safe for concurrent use.
type Table struct {
	me      ID
	root    *zone
	ips     map[netip.Addr]int
	subnets map[[3]byte]int
	count   int

	// MyIP returns our public IP (for verify keys); may be nil.
	MyIP func() netip.Addr
}

// NewTable returns an empty table for the node with ID me.
func NewTable(me ID, now time.Time) *Table {
	t := &Table{me: me, ips: map[netip.Addr]int{}, subnets: map[[3]byte]int{}}
	t.root = t.newZone(nil, 0, ID{}, now)
	return t
}

func (t *Table) newZone(parent *zone, level int, prefix ID, now time.Time) *zone {
	z := &zone{parent: parent, level: level, prefix: prefix}
	// Stagger the small timers as eMule does: by the zone index (its path
	// read as a number), here its low bits.
	idx := 0
	for i := max(0, level-6); i < level; i++ {
		idx = idx<<1 | prefix.Bit(i)
	}
	z.nextSmall = now.Add(time.Duration(idx%60) * time.Second)
	z.nextBig = now.Add(10 * time.Second)
	return z
}

func (t *Table) myIP() netip.Addr {
	if t.MyIP == nil {
		return netip.Addr{}
	}
	return t.MyIP()
}

// Me returns our ID.
func (t *Table) Me() ID { return t.me }

// Len returns the number of contacts.
func (t *Table) Len() int { return t.count }

func (t *Table) leafFor(id ID) *zone {
	d := t.me.Xor(id)
	z := t.root
	for !z.leaf() {
		z = z.sub[d.Bit(z.level)]
	}
	return z
}

// Get returns the contact with id, or nil.
func (t *Table) Get(id ID) *Contact {
	for _, c := range t.leafFor(id).bin {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// GetByAddr returns a contact at ip with the given UDP port (any port if
// 0), or nil.
func (t *Table) GetByAddr(ip netip.Addr, port uint16) *Contact {
	var found *Contact
	t.walk(func(c *Contact) bool {
		if c.IP == ip && (port == 0 || c.UDPPort == port) {
			found = c
			return false
		}
		return true
	})
	return found
}

// walk calls f for every contact until f returns false.
func (t *Table) walk(f func(*Contact) bool) {
	var rec func(z *zone) bool
	rec = func(z *zone) bool {
		if z.leaf() {
			for _, c := range z.bin {
				if !f(c) {
					return false
				}
			}
			return true
		}
		return rec(z.sub[0]) && rec(z.sub[1])
	}
	rec(t.root)
}

// All returns copies of every contact.
func (t *Table) All() []Contact {
	var out []Contact
	t.walk(func(c *Contact) bool { out = append(out, *c); return true })
	return out
}

func (t *Table) globalLimitsOK(ip netip.Addr) bool {
	if t.ips[ip] >= maxContactsIP {
		return false
	}
	return t.subnets[subnet(ip)] < maxContactsSubnet
}

func (t *Table) track(ip netip.Addr, delta int) {
	if n := t.ips[ip] + delta; n > 0 {
		t.ips[ip] = n
	} else {
		delete(t.ips, ip)
	}
	s := subnet(ip)
	if n := t.subnets[s] + delta; n > 0 {
		t.subnets[s] = n
	} else {
		delete(t.subnets, s)
	}
}

// binAdd adds c to leaf z if the limits allow it.
func (t *Table) binAdd(z *zone, c *Contact) bool {
	same := 0
	for _, o := range z.bin {
		if o.ID == c.ID {
			return false
		}
		if subnet(o.IP) == subnet(c.IP) {
			same++
		}
	}
	if !t.globalLimitsOK(c.IP) || same >= maxBinSubnet || len(z.bin) >= K {
		return false
	}
	z.bin = append(z.bin, c)
	t.track(c.IP, 1)
	t.count++
	return true
}

func (t *Table) binRemove(z *zone, c *Contact) {
	for i, o := range z.bin {
		if o == c {
			z.bin = append(z.bin[:i], z.bin[i+1:]...)
			t.track(c.IP, -1)
			t.count--
			return
		}
	}
}

func pushToBottom(z *zone, c *Contact) {
	for i, o := range z.bin {
		if o == c {
			copy(z.bin[i:], z.bin[i+1:])
			z.bin[len(z.bin)-1] = c
			return
		}
	}
}

// changeIP moves c to newIP if the limits allow it.
func (t *Table) changeIP(z *zone, c *Contact, newIP netip.Addr) bool {
	if c.IP == newIP {
		return true
	}
	if t.ips[newIP] >= maxContactsIP {
		return false
	}
	if subnet(c.IP) != subnet(newIP) {
		if t.subnets[subnet(newIP)] >= maxContactsSubnet {
			return false
		}
		same := 0
		for _, o := range z.bin {
			if subnet(o.IP) == subnet(newIP) {
				same++
			}
		}
		if same >= maxBinSubnet {
			return false
		}
	}
	t.track(c.IP, -1)
	c.IP = newIP
	c.Verified = false // proven for the old IP, not this one
	t.track(c.IP, 1)
	return true
}

// Add adds a contact, or with update refreshes the one with the same ID.
// It returns whether the contact was added or updated, and whether the
// stored contact's IP is verified. Addresses that cannot be Kad nodes on
// the Internet are refused.
func (t *Table) Add(c Contact, update, fromHello bool, now time.Time) (bool, bool) {
	if !GoodAddr(c.IP, c.UDPPort) || (c.UDPPort == 53 && c.Version <= 5) {
		return false, false
	}
	return t.AddUnfiltered(c, update, fromHello, now)
}

// AddUnfiltered is Add without the address check.
func (t *Table) AddUnfiltered(c Contact, update, fromHello bool, now time.Time) (bool, bool) {
	c.IP = c.IP.Unmap()
	if c.ID == t.me || c.Version <= 1 || !c.IP.Is4() {
		return false, false
	}
	nc := newContact(c, now)
	nc.helloReceived = fromHello
	z := t.leafFor(c.ID)
	for {
		var old *Contact
		for _, o := range z.bin {
			if o.ID == c.ID {
				old = o
				break
			}
		}
		switch {
		case old != nil:
			if !update {
				return false, old.Verified
			}
			myIP := t.myIP()
			oldKey := old.Key.For(myIP)
			switch {
			case oldKey != 0 && oldKey != c.Key.For(myIP):
				// It proved its identity with another key: whoever sends
				// this may not be that node.
				return false, old.Verified
			case old.Version >= 1 && old.Version < 6 && old.helloReceived:
				// Nodes without keys may not move.
				if old.IP != c.IP || old.TCPPort != c.TCPPort || old.Version != c.Version || old.UDPPort != c.UDPPort {
					return false, old.Verified
				}
				t.setAlive(z, old, now)
				return true, old.Verified
			default:
				if c.Version < old.Version || !t.changeIP(z, old, c.IP) {
					return false, old.Verified
				}
				old.UDPPort, old.TCPPort, old.Version, old.Key = c.UDPPort, c.TCPPort, c.Version, c.Key
				old.Verified = old.Verified || c.Verified
				if fromHello {
					old.helloReceived = true
				}
				t.setAlive(z, old, now)
				return true, old.Verified
			}
		case len(z.bin) < K:
			return t.binAdd(z, nc), nc.Verified
		case z.canSplit():
			t.split(z, now)
			z = z.sub[t.me.Xor(c.ID).Bit(z.level)]
		default:
			return false, false
		}
	}
}

func (t *Table) setAlive(z *zone, c *Contact, now time.Time) {
	c.updateType(now)
	pushToBottom(z, c)
}

// SetAlive marks the contact with id as alive (it answered).
func (t *Table) SetAlive(id ID, now time.Time) {
	z := t.leafFor(id)
	for _, c := range z.bin {
		if c.ID == id {
			t.setAlive(z, c, now)
			return
		}
	}
}

func (t *Table) split(z *zone, now time.Time) {
	for side := range 2 {
		p := z.prefix
		p.SetBit(z.level, side)
		z.sub[side] = t.newZone(z, z.level+1, p, now)
	}
	entries := z.bin
	z.bin = nil
	for _, c := range entries {
		t.track(c.IP, -1)
		t.count--
		t.binAdd(z.sub[t.me.Xor(c.ID).Bit(z.level)], c)
	}
}

// Consolidate merges sibling leaves that hold fewer than K/2 contacts
// together. It returns how many merges were done.
func (t *Table) Consolidate(now time.Time) int {
	var rec func(z *zone) int
	rec = func(z *zone) int {
		if z.leaf() {
			return 0
		}
		n := rec(z.sub[0]) + rec(z.sub[1])
		if z.sub[0].leaf() && z.sub[1].leaf() && len(z.sub[0].bin)+len(z.sub[1].bin) < K/2 {
			entries := append(append([]*Contact(nil), z.sub[0].bin...), z.sub[1].bin...)
			z.sub = [2]*zone{}
			z.bin = nil
			z.nextBig = now.Add(10 * time.Second)
			for _, c := range entries {
				t.track(c.IP, -1)
				t.count--
				t.binAdd(z, c)
			}
			n++
		}
		return n
	}
	return rec(t.root)
}

// Verify marks the contact id at ip as verified. It returns false when
// there is no such contact at that IP.
func (t *Table) Verify(id ID, ip netip.Addr) bool {
	c := t.Get(id)
	if c == nil || c.IP != ip.Unmap() {
		return false
	}
	c.Verified = true
	return true
}

// SetAllVerified marks every contact as verified (a nodes.dat from a
// version that did not save the flag).
func (t *Table) SetAllVerified() {
	t.walk(func(c *Contact) bool { c.Verified = true; return true })
}

// Acceptable reports whether a contact learned from another node's answer
// may be used in a search.
func (t *Table) Acceptable(c Contact) bool {
	if c.Version <= 1 {
		return false
	}
	if d := t.Get(c.ID); d != nil {
		return !(d.Verified && d.IP != c.IP.Unmap()) && d.UDPPort == c.UDPPort
	}
	return t.globalLimitsOK(c.IP.Unmap())
}

// Closest returns up to n verified contacts of type maxType or better,
// closest to target first.
func (t *Table) Closest(target ID, maxType, n int) []Contact {
	var out []Contact
	t.walk(func(c *Contact) bool {
		if c.typ <= maxType && c.Verified {
			out = append(out, *c)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID.Xor(target).Cmp(out[j].ID.Xor(target)) < 0
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// RandomContact returns a random contact of type maxType or better and
// version minVersion or later, or nil.
func (t *Table) RandomContact(maxType int, minVersion byte) *Contact {
	var fit []*Contact
	t.walk(func(c *Contact) bool {
		if c.typ <= maxType && c.Version >= minVersion {
			fit = append(fit, c)
		}
		return true
	})
	if len(fit) == 0 {
		return nil
	}
	return fit[rand.IntN(len(fit))]
}

// BootstrapContacts returns up to n contacts spread over the ID space
// (eMule's TopDepth(5)): what we give nodes that bootstrap from us and
// what we save in nodes.dat.
func (t *Table) BootstrapContacts(n int) []Contact {
	var out []Contact
	var rec func(z *zone, depth int)
	rec = func(z *zone, depth int) {
		switch {
		case z.leaf():
			for _, c := range z.bin {
				out = append(out, *c)
			}
		case depth <= 0:
			rec(z.sub[rand.IntN(2)], 0)
		default:
			rec(z.sub[0], depth-1)
			rec(z.sub[1], depth-1)
		}
	}
	rec(t.root, 5)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Timers is what the routing table asks for on a tick.
type Timers struct {
	Hello  []Contact // contacts to send a HELLO_REQ to
	Lookup []ID      // random node lookups to refill thin zones
}

// Tick runs the zones' timers: every minute each leaf drops contacts whose
// check expired and checks its oldest contact; every hour each leaf that
// is not full looks up a random ID in its range (one lookup per 10 s for
// the whole table, which bigDue says is allowed now). It reports whether
// a lookup was started.
func (t *Table) Tick(now time.Time, bigDue bool) (Timers, bool) {
	var out Timers
	var leaves []*zone
	var rec func(z *zone)
	rec = func(z *zone) {
		if z.leaf() {
			leaves = append(leaves, z)
			return
		}
		rec(z.sub[0])
		rec(z.sub[1])
	}
	rec(t.root)
	started := false
	for _, z := range leaves {
		if bigDue && !started && !z.nextBig.After(now) {
			if z.indexBelow(kK) || z.level < kBase || K-len(z.bin) >= K*8/10 {
				out.Lookup = append(out.Lookup, t.me.Xor(z.prefix.WithPrefix(z.level)))
				z.nextBig = now.Add(time.Hour)
				started = true
			}
		}
		if !z.nextSmall.After(now) {
			z.nextSmall = now.Add(time.Minute)
			if c := t.smallTimer(z, now); c != nil {
				out.Hello = append(out.Hello, *c)
			}
		}
	}
	return out, started
}

func (t *Table) smallTimer(z *zone, now time.Time) *Contact {
	for _, c := range append([]*Contact(nil), z.bin...) {
		if c.typ == 4 && !c.expires.IsZero() && !c.expires.After(now) {
			t.binRemove(z, c)
			continue
		}
		if c.expires.IsZero() {
			c.expires = now
		}
	}
	if len(z.bin) == 0 {
		return nil
	}
	c := z.bin[0]
	if !c.expires.Before(now) || c.typ == 4 {
		pushToBottom(z, c)
		return nil
	}
	c.checkingType(now)
	if c.Version < 2 {
		return nil
	}
	return c
}

// Depth returns the depth of the deepest zone (for status).
func (t *Table) Depth() int {
	var rec func(z *zone) int
	rec = func(z *zone) int {
		if z.leaf() {
			return 0
		}
		return 1 + max(rec(z.sub[0]), rec(z.sub[1]))
	}
	return rec(t.root)
}
