package kad

import (
	"bytes"
	"fmt"
	"net/netip"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// pubIP returns distinct public addresses: 1.0.0.0 + i*256 (one per /24)
// or, with sameNet, 1.2.3.i.
func pubIP(i int, sameNet bool) netip.Addr {
	if sameNet {
		return netip.AddrFrom4([4]byte{1, 2, 3, byte(i)})
	}
	return netip.AddrFrom4([4]byte{1 + byte(i>>16), byte(i >> 8), byte(i), 9})
}

func contactN(i int) Contact {
	return Contact{ID: RandomID(), IP: pubIP(i, false), UDPPort: 4672, TCPPort: 4662, Version: 8, Verified: true}
}

func TestGoodAddr(t *testing.T) {
	for s, want := range map[string]bool{
		"1.2.3.4": true, "93.184.216.34": true,
		"10.0.0.1": false, "192.168.1.5": false, "172.16.0.1": false, "127.0.0.1": false,
		"0.1.2.3": false, "224.0.0.1": false, "255.255.255.255": false, "169.254.1.1": false,
		"100.64.0.1": false, "198.18.0.1": false, "203.0.113.5": false, "::1": false, "2001:db8::1": false,
	} {
		if got := GoodAddr(netip.MustParseAddr(s), 4672); got != want {
			t.Errorf("%s: %v", s, got)
		}
	}
	if GoodAddr(netip.MustParseAddr("1.2.3.4"), 0) {
		t.Error("port 0 accepted")
	}
}

func TestTableAddBasics(t *testing.T) {
	me := RandomID()
	tb := NewTable(me, t0)
	c := contactN(1)
	if ok, _ := tb.Add(c, false, false, t0); !ok || tb.Len() != 1 || tb.Get(c.ID) == nil {
		t.Fatal("not added")
	}
	if ok, _ := tb.Add(c, false, false, t0); ok || tb.Len() != 1 {
		t.Fatal("added twice")
	}
	self := contactN(2)
	self.ID = me
	old := contactN(3)
	old.Version = 1
	lan := contactN(4)
	lan.IP = netip.MustParseAddr("192.168.0.4")
	dns := contactN(5)
	dns.UDPPort, dns.Version = 53, 5
	for name, x := range map[string]Contact{"self": self, "kad1": old, "lan": lan, "dns": dns} {
		if ok, _ := tb.Add(x, false, false, t0); ok {
			t.Errorf("%s added", name)
		}
	}
	// One contact per IP.
	sameIP := contactN(1)
	if ok, _ := tb.Add(sameIP, false, false, t0); ok {
		t.Fatal("second contact on one IP added")
	}
	if tb.GetByAddr(c.IP, 4672) == nil || tb.GetByAddr(c.IP, 1) != nil || tb.GetByAddr(c.IP, 0) == nil {
		t.Fatal("GetByAddr")
	}
}

// At most 2 contacts of one /24 per bin, and 10 in the whole table.
func TestTableSubnetLimits(t *testing.T) {
	me := RandomID()
	tb := NewTable(me, t0)
	added := 0
	for i := 1; i < 200; i++ {
		c := Contact{ID: RandomID(), IP: pubIP(i, true), UDPPort: 4672, Version: 8, Verified: true}
		if ok, _ := tb.Add(c, false, false, t0); ok {
			added++
		}
	}
	if added > maxContactsSubnet || added < 2 {
		t.Fatalf("%d contacts from one /24", added)
	}
	var check func(z *zone)
	check = func(z *zone) {
		if !z.leaf() {
			check(z.sub[0])
			check(z.sub[1])
			return
		}
		if len(z.bin) > maxBinSubnet {
			t.Fatalf("bin with %d contacts of one /24", len(z.bin))
		}
	}
	check(tb.root)
}

// Bins split near our ID and in the first zones of each level; every
// contact sits in the leaf its distance leads to.
func TestTableSplits(t *testing.T) {
	me := RandomID()
	tb := NewTable(me, t0)
	for i := 1; i <= 5000; i++ {
		tb.Add(contactN(i), false, false, t0)
	}
	if tb.Len() < 100 || tb.Depth() < 4 {
		t.Fatalf("%d contacts, depth %d", tb.Len(), tb.Depth())
	}
	n := 0
	var check func(z *zone)
	check = func(z *zone) {
		if !z.leaf() {
			if !(z.level < kBase || z.indexBelow(kK)) {
				t.Fatalf("zone at level %d split though far from us", z.level)
			}
			check(z.sub[0])
			check(z.sub[1])
			return
		}
		if len(z.bin) > K {
			t.Fatalf("bin of %d", len(z.bin))
		}
		for _, c := range z.bin {
			d := me.Xor(c.ID)
			for i := 0; i < z.level; i++ {
				if d.Bit(i) != z.prefix.Bit(i) {
					t.Fatalf("contact in the wrong zone")
				}
			}
			n++
		}
	}
	check(tb.root)
	if n != tb.Len() || len(tb.ips) != n {
		t.Fatalf("counted %d, Len %d, tracked IPs %d", n, tb.Len(), len(tb.ips))
	}
}

func TestIndexBelow(t *testing.T) {
	for _, tc := range []struct {
		bits string
		want bool
	}{{"", true}, {"1", true}, {"100", true}, {"101", false}, {"0100", true}, {"0101", false}, {"1000", false}, {"00000011", true}} {
		z := &zone{level: len(tc.bits)}
		for i, b := range tc.bits {
			z.prefix.SetBit(i, int(b-'0'))
		}
		if got := z.indexBelow(kK); got != tc.want {
			t.Errorf("%q: %v", tc.bits, got)
		}
	}
}

func TestTableClosest(t *testing.T) {
	tb := NewTable(RandomID(), t0)
	var ids []ID
	for i := 1; i <= 300; i++ {
		c := contactN(i)
		c.Verified = i%3 != 0
		if ok, _ := tb.Add(c, false, false, t0); ok {
			ids = append(ids, c.ID)
		}
	}
	target := RandomID()
	got := tb.Closest(target, 3, 20)
	if len(got) != 20 {
		t.Fatalf("%d contacts", len(got))
	}
	for i, c := range got {
		if !c.Verified {
			t.Fatal("unverified contact returned")
		}
		if i > 0 && got[i-1].ID.Xor(target).Cmp(c.ID.Xor(target)) > 0 {
			t.Fatal("not sorted by distance")
		}
	}
	// Nothing verified and closer was left out.
	worst := got[len(got)-1].ID.Xor(target)
	for _, c := range tb.All() {
		if c.Verified && c.ID.Xor(target).Cmp(worst) < 0 {
			found := false
			for _, g := range got {
				found = found || g.ID == c.ID
			}
			if !found {
				t.Fatal("a closer contact was left out")
			}
		}
	}
	if len(tb.Closest(target, -1, 20)) != 0 {
		t.Fatal("maxType ignored")
	}
}

func TestTableUpdate(t *testing.T) {
	myIP := netip.MustParseAddr("5.6.7.8")
	tb := NewTable(RandomID(), t0)
	tb.MyIP = func() netip.Addr { return myIP }
	c := contactN(1)
	c.Verified = false
	c.Key = UDPKey{Key: 111, IP: myIP}
	tb.Add(c, false, false, t0)

	// Another key for the same ID: not that node.
	forged := c
	forged.IP = pubIP(2, false)
	forged.Key = UDPKey{Key: 222, IP: myIP}
	if ok, _ := tb.Add(forged, true, true, t0); ok || tb.Get(c.ID).IP != c.IP {
		t.Fatal("contact taken over with another key")
	}
	// The same key from a new IP: it moved.
	moved := c
	moved.IP = pubIP(3, false)
	tb.Get(c.ID).Verified = true
	if ok, ver := tb.Add(moved, true, true, t0.Add(time.Minute)); !ok || ver || tb.Get(c.ID).IP != moved.IP {
		t.Fatalf("move: added %v, verified %v", ok, ver)
	}
	// Proven for the old IP, not the new one.
	if tb.Get(c.ID).Verified {
		t.Fatal("still verified after moving")
	}
	moved.Verified = true
	if _, ver := tb.Add(moved, true, true, t0.Add(2*time.Minute)); !ver {
		t.Fatal("not verified by a proof from the new IP")
	}
	if tb.ips[c.IP] != 0 || tb.ips[moved.IP] != 1 {
		t.Fatal("IP tracking not updated")
	}
	// An older version does not replace a newer one.
	older := moved
	older.Version = 6
	if ok, _ := tb.Add(older, true, false, t0); ok {
		t.Fatal("downgraded")
	}
	// A legacy node (no keys) that said hello may not move.
	tb2 := NewTable(RandomID(), t0)
	leg := contactN(4)
	leg.Version = 5
	tb2.Add(leg, false, true, t0)
	legMoved := leg
	legMoved.IP = pubIP(5, false)
	if ok, _ := tb2.Add(legMoved, true, true, t0); ok || tb2.Get(leg.ID).IP != leg.IP {
		t.Fatal("legacy contact moved")
	}
	if ok, _ := tb2.Add(leg, true, true, t0); !ok {
		t.Fatal("legacy contact not refreshed")
	}
	if !tb2.Verify(leg.ID, leg.IP) || tb2.Verify(leg.ID, legMoved.IP) || tb2.Verify(RandomID(), leg.IP) {
		t.Fatal("Verify")
	}
}

func TestContactAging(t *testing.T) {
	tb := NewTable(RandomID(), t0)
	c := contactN(1)
	tb.Add(c, false, false, t0)
	got := tb.Get(c.ID)
	if got.Type() != 3 {
		t.Fatalf("new contact type %d", got.Type())
	}
	tb.SetAlive(c.ID, t0.Add(10*time.Minute))
	if got.Type() != 2 {
		t.Fatalf("alive after 10 min: type %d", got.Type())
	}
	tb.SetAlive(c.ID, t0.Add(90*time.Minute))
	if got.Type() != 1 {
		t.Fatalf("alive after 1.5 h: type %d", got.Type())
	}
	tb.SetAlive(c.ID, t0.Add(3*time.Hour))
	if got.Type() != 0 {
		t.Fatalf("alive after 3 h: type %d", got.Type())
	}
}

// Every minute a leaf checks its oldest contact; one that never answers
// reaches type 4 and is dropped when that check expires.
func TestSmallTimerDropsDeadContacts(t *testing.T) {
	tb := NewTable(RandomID(), t0)
	c := contactN(1)
	tb.Add(c, false, false, t0)
	now := t0
	hellos := 0
	for i := 0; i < 20 && tb.Len() > 0; i++ {
		now = now.Add(time.Minute)
		timers, _ := tb.Tick(now, false)
		hellos += len(timers.Hello)
	}
	if tb.Len() != 0 {
		t.Fatalf("dead contact kept (type %d)", tb.Get(c.ID).Type())
	}
	if hellos == 0 || len(tb.ips) != 0 {
		t.Fatalf("%d hellos, %d IPs tracked", hellos, len(tb.ips))
	}
	// One that answers stays.
	tb.Add(c, false, false, now)
	for i := 0; i < 30; i++ {
		now = now.Add(time.Minute)
		timers, _ := tb.Tick(now, false)
		for _, h := range timers.Hello {
			tb.SetAlive(h.ID, now)
		}
	}
	if tb.Len() != 1 {
		t.Fatal("live contact dropped")
	}
}

func TestBigTimerLookups(t *testing.T) {
	me := RandomID()
	tb := NewTable(me, t0)
	_, started := tb.Tick(t0, true)
	if started {
		t.Fatal("lookup before the zone's first 10 s")
	}
	timers, started := tb.Tick(t0.Add(11*time.Second), true)
	if !started || len(timers.Lookup) != 1 {
		t.Fatalf("no lookup: %+v", timers)
	}
	if _, started := tb.Tick(t0.Add(12*time.Second), true); started {
		t.Fatal("same zone looked up again within the hour")
	}
	if _, started := tb.Tick(t0.Add(2*time.Hour), false); started {
		t.Fatal("lookup while not allowed")
	}
}

func TestConsolidate(t *testing.T) {
	tb := NewTable(RandomID(), t0)
	var ids []ID
	for i := 1; i <= 400; i++ {
		c := contactN(i)
		if ok, _ := tb.Add(c, false, false, t0); ok {
			ids = append(ids, c.ID)
		}
	}
	depth := tb.Depth()
	// Remove most contacts, then merge.
	for _, id := range ids[3:] {
		z := tb.leafFor(id)
		tb.binRemove(z, tb.Get(id))
	}
	if n := tb.Consolidate(t0); n == 0 || tb.Depth() >= depth {
		t.Fatalf("%d merges, depth %d -> %d", n, depth, tb.Depth())
	}
	if tb.Len() != 3 {
		t.Fatalf("%d contacts after merging", tb.Len())
	}
	for _, id := range ids[:3] {
		if tb.Get(id) == nil {
			t.Fatal("contact lost in merging")
		}
	}
}

func TestBootstrapContacts(t *testing.T) {
	tb := NewTable(RandomID(), t0)
	for i := 1; i <= 2000; i++ {
		tb.Add(contactN(i), false, false, t0)
	}
	if got := tb.BootstrapContacts(20); len(got) != 20 {
		t.Fatalf("%d", len(got))
	}
	if got := tb.BootstrapContacts(1000); len(got) < 50 {
		t.Fatalf("only %d", len(got))
	}
}

func TestNodesFile(t *testing.T) {
	var cs []Contact
	for i := 1; i <= 30; i++ {
		c := contactN(i)
		c.Key = UDPKey{Key: uint32(i), IP: netip.MustParseAddr("5.6.7.8")}
		c.Verified = i%2 == 0
		cs = append(cs, c)
	}
	b := EncodeNodes(cs)
	f, err := ParseNodes(b)
	if err != nil || len(f.Contacts) != 30 || f.Bootstrap || !f.HasVerified {
		t.Fatalf("%v %d", err, len(f.Contacts))
	}
	for i, c := range f.Contacts {
		if c.ID != cs[i].ID || c.IP != cs[i].IP || c.UDPPort != 4672 || c.TCPPort != 4662 || c.Version != 8 || c.Key != cs[i].Key || c.Verified != cs[i].Verified {
			t.Fatalf("contact %d: %+v", i, c)
		}
	}
	// The node's IP is a number (1.0.1.9 = 0x01000109 little-endian);
	// the key's IP is in network order.
	if !bytes.Equal(b[12+16:12+20], []byte{9, 1, 0, 1}) {
		t.Fatalf("IP bytes % X", b[12+16:12+20])
	}
	if !bytes.Equal(b[12+29:12+33], []byte{5, 6, 7, 8}) {
		t.Fatalf("key IP bytes % X", b[12+29:12+33])
	}
	// Version 1 (no keys), version 3 bootstrap, old and broken files.
	v1 := []byte{0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0}
	id := cs[0].ID.Wire()
	v1 = append(v1, id[:]...)
	v1 = append(v1, 9, 1, 0, 1, 0x40, 0x12, 0x36, 0x12, 8)
	if f, err := ParseNodes(v1); err != nil || len(f.Contacts) != 1 || f.HasVerified || f.Contacts[0].UDPPort != 0x1240 {
		t.Fatalf("v1: %v %+v", err, f)
	}
	v3 := append([]byte{0, 0, 0, 0, 3, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0}, v1[12:]...)
	if f, err := ParseNodes(v3); err != nil || !f.Bootstrap || len(f.Contacts) != 1 {
		t.Fatalf("bootstrap: %v %+v", err, f)
	}
	for name, bad := range map[string][]byte{
		"v0":        {5, 0, 0, 0},
		"version 9": {0, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0},
		"truncated": b[:len(b)-1],
		"count lie": {0, 0, 0, 0, 2, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0x7F},
		"empty":     {},
	} {
		if _, err := ParseNodes(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	_ = fmt.Sprint()
}
