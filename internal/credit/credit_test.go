package credit

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// Our PKCS #1 v1.5 code and key encodings give what Go's crypto/rsa and
// crypto/x509 give, on a key size those accept.
func TestRSAMatchesStandardLibrary(t *testing.T) {
	gk, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	k := &privateKey{publicKey{gk.N, gk.E}, gk.D, gk.Primes[0], gk.Primes[1]}
	msg := []byte("a public key and a challenge")
	h := sha1.Sum(msg)
	want, err := rsa.SignPKCS1v15(nil, gk, crypto.SHA1, h[:])
	if err != nil {
		t.Fatal(err)
	}
	if got := k.sign(msg); !bytes.Equal(got, want) {
		t.Fatal("signature differs from crypto/rsa")
	}
	if !k.publicKey.verify(msg, want) || k.publicKey.verify(append(msg, 0), want) {
		t.Fatal("verify")
	}
	spki, _ := x509.MarshalPKIXPublicKey(&gk.PublicKey)
	if !bytes.Equal(k.publicKey.marshal(), spki) {
		t.Fatal("public key encoding differs from crypto/x509")
	}
	p8, _ := x509.MarshalPKCS8PrivateKey(gk)
	if pk, err := parsePrivateKey(p8); err != nil || pk.D.Cmp(gk.D) != 0 {
		t.Fatalf("parse PKCS #8 from crypto/x509: %v", err)
	}
	mine, _ := k.marshal()
	if back, err := x509.ParsePKCS8PrivateKey(mine); err != nil || back.(*rsa.PrivateKey).D.Cmp(gk.D) != 0 {
		t.Fatalf("crypto/x509 reading ours: %v", err)
	}
}

func TestGeneratedKey(t *testing.T) {
	k, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}
	if k.N.BitLen() != 384 || len(k.publicKey.marshal()) != 76 {
		t.Fatalf("%d bits, %d byte key", k.N.BitLen(), len(k.publicKey.marshal()))
	}
	sig := k.sign([]byte("x"))
	pub, err := parsePublicKey(k.publicKey.marshal())
	if err != nil || !pub.verify([]byte("x"), sig) || pub.verify([]byte("y"), sig) {
		t.Fatal("round trip", err)
	}
	der, _ := k.marshal()
	if back, err := parsePrivateKey(der); err != nil || back.D.Cmp(k.D) != 0 {
		t.Fatal(err)
	}
	if pub.verify([]byte("x"), k.N.FillBytes(make([]byte, 48))) {
		t.Fatal("signature >= N accepted")
	}
}

func TestRatio(t *testing.T) {
	const mb = 1 << 20
	for _, c := range []struct {
		up, down uint64
		want     float64
	}{
		{0, 0, 1},
		{0, mb - 1, 1},
		{0, 2 * mb, 2},         // sqrt(2+2)
		{0, 98 * mb, 10},       // sqrt(100)
		{0, 1000 * mb, 10},     // capped
		{100 * mb, 30 * mb, 1}, // 60/100, at least 1
		{20 * mb, 30 * mb, 3},  // 2*30/20
	} {
		if got := ratio(c.up, c.down); got != c.want {
			t.Errorf("ratio(%d, %d) = %v, want %v", c.up, c.down, got, c.want)
		}
	}
}

type end struct {
	st  *Store
	id  *Ident
	out []*proto.Packet
}

func newEnd(t *testing.T) *end {
	st, err := Open(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return &end{st: st}
}

func hello(h ed2k.Hash, sec byte) peer.Hello {
	return peer.Hello{UserHash: h, Tags: proto.TagList{proto.IntTag(peer.CTEMuleMiscOpts1, uint32(sec)<<16)}}
}

// connect makes the two ends talk until neither has anything to say.
func connect(t *testing.T, a, b *end, ha, hb ed2k.Hash, secA, secB byte, ipA, ipB net.IP) {
	t.Helper()
	mk := func(e *end) func(byte, []byte) error {
		return func(op byte, p []byte) error {
			e.out = append(e.out, &proto.Packet{Proto: proto.ProtoEMule, Opcode: op, Payload: p})
			return nil
		}
	}
	a.out, b.out = nil, nil
	a.id = a.st.NewIdent(hello(hb, secB), ipB, func() net.IP { return ipA }, mk(a))
	b.id = b.st.NewIdent(hello(ha, secA), ipA, func() net.IP { return ipB }, mk(b))
	a.id.Start()
	b.id.Start()
	for range 10 {
		ao, bo := a.out, b.out
		a.out, b.out = nil, nil
		for _, p := range ao {
			if ok, err := b.id.Handle(p); !ok || err != nil {
				t.Fatal(ok, err)
			}
		}
		for _, p := range bo {
			if ok, err := a.id.Handle(p); !ok || err != nil {
				t.Fatal(ok, err)
			}
		}
	}
}

func TestIdentBothWays(t *testing.T) {
	ha, hb := ed2k.Hash{1}, ed2k.Hash{2}
	ipA, ipB := net.IPv4(11, 0, 0, 1), net.IPv4(11, 0, 0, 2)
	for _, sec := range [][2]byte{{3, 3}, {2, 2}, {3, 2}} {
		a, b := newEnd(t), newEnd(t)
		connect(t, a, b, ha, hb, sec[0], sec[1], ipA, ipB)
		if !a.st.Identified(hb, ipB) || !b.st.Identified(ha, ipA) {
			t.Fatalf("%v: not identified", sec)
		}
		// Next connection: the stored key is used, only a signature asked.
		b.st.recs[ha].identIP = ""
		connect(t, &end{st: a.st}, &end{st: b.st}, ha, hb, sec[0], sec[1], ipA, ipB)
		if !b.st.Identified(ha, ipA) {
			t.Fatalf("%v: not identified with the stored key", sec)
		}
	}
	// Without support on one side, or with version 1 only (signatures
	// without an address), nothing is said.
	for _, sec := range []byte{0, 1} {
		a, b := newEnd(t), newEnd(t)
		connect(t, a, b, ha, hb, sec, 3, ipA, ipB)
		if b.st.Identified(ha, ipA) || a.st.Identified(hb, ipB) {
			t.Fatalf("identified with support %d", sec)
		}
	}
}

// Someone else with the same user hash (another key) gets nothing:
// neither identification nor the credits.
func TestIdentImpostor(t *testing.T) {
	ha, hb := ed2k.Hash{1}, ed2k.Hash{2}
	ipA, ipB, ipX := net.IPv4(11, 0, 0, 1), net.IPv4(11, 0, 0, 2), net.IPv4(11, 0, 0, 3)
	a, b := newEnd(t), newEnd(t)
	connect(t, a, b, ha, hb, 3, 3, ipA, ipB)
	b.st.Downloaded(ha, ipA, 200<<20)
	if r := b.st.Ratio(ha, ipA); r != 10 {
		t.Fatalf("ratio %v", r)
	}
	x := newEnd(t) // claims hash ha, with its own key
	connect(t, x, b, ha, hb, 3, 3, ipX, ipB)
	if b.st.Identified(ha, ipX) || b.st.Ratio(ha, ipX) != 1 {
		t.Fatal("impostor identified")
	}
	b.st.Uploaded(ha, ipX, 100<<20)
	if c, _ := b.st.Get(ha, ipA); c.Uploaded != 0 {
		t.Fatal("impostor used up the credits")
	}
	// A made-up signature is refused.
	y := b.st.NewIdent(hello(ed2k.Hash{9}, 3), ipX, nil, func(byte, []byte) error { return nil })
	y.Start()
	y.Handle(&proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpPublicKey, Payload: append([]byte{76}, a.st.PublicKey()...)})
	y.Handle(&proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpSignature, Payload: append([]byte{48}, make([]byte, 48)...)})
	if b.st.Identified(ed2k.Hash{9}, ipX) {
		t.Fatal("forged signature accepted")
	}
}

func TestStoreSaveLoad(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.IPv4(11, 0, 0, 1)
	k := []byte("0123456789")
	st.verified(ed2k.Hash{1}, ip, k)
	st.Downloaded(ed2k.Hash{1}, ip, 5<<20)
	st.Uploaded(ed2k.Hash{1}, ip, 1<<20)
	st.verified(ed2k.Hash{2}, ip, k)
	st.recs[ed2k.Hash{2}].seen = time.Now().Add(-200 * 24 * time.Hour).Unix()
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(st2.PublicKey(), st.PublicKey()) {
		t.Fatal("key not kept")
	}
	c, ok := st2.Get(ed2k.Hash{1}, ip)
	if !ok || c.Downloaded != 5<<20 || c.Uploaded != 1<<20 || c.Identified {
		t.Fatalf("%+v %v", c, ok)
	}
	if _, ok := st2.Get(ed2k.Hash{2}, ip); ok {
		t.Fatal("expired credits loaded")
	}
	if !bytes.Equal(st2.KeyOf(ed2k.Hash{1}), k) {
		t.Fatal("key of peer not kept")
	}
	// A damaged credits file: start with none. A damaged key: an error.
	os.WriteFile(filepath.Join(dir, "credits.dat"), []byte("MMCR\x01\xff\xff\xff\xff"), 0o600)
	if st3, err := Open(dir, t.Logf); err != nil || st3.Len() != 0 {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "cryptkey.dat"), []byte("bm9wZQ=="), 0o600)
	if _, err := Open(dir, t.Logf); err == nil {
		t.Fatal("damaged key accepted")
	}
}

func TestStoreBounded(t *testing.T) {
	st, _ := Open(t.TempDir(), t.Logf)
	ip := net.IPv4(11, 0, 0, 1)
	for i := range maxRecords + 5 {
		var h ed2k.Hash
		h[0], h[1], h[2] = byte(i), byte(i>>8), byte(i>>16)
		st.verified(h, ip, []byte("k"))
	}
	if st.Len() != maxRecords {
		t.Fatal(st.Len())
	}
}

// sigFrom makes the signature end e would send for challenge c of store
// s, of the given form: kind 0 = version 1 (no address).
func sigFrom(e *end, s *Store, c uint32, kind byte, ip net.IP) []byte {
	msg := binary.LittleEndian.AppendUint32(append([]byte(nil), s.PublicKey()...), c)
	if kind != 0 {
		msg = append(append(msg, ip.To4()...), kind)
	}
	sig := e.st.key.sign(msg)
	out := append([]byte{byte(len(sig))}, sig...)
	if kind != 0 {
		out = append(out, kind)
	}
	return out
}

// Only version 2 signatures over this connection's addresses prove an
// identity: a version 1 one, or one over no address, could be someone
// else's passed on. A good one does.
func TestIdentSignatureForms(t *testing.T) {
	ha := ed2k.Hash{1}
	ipA, ipB := net.IPv4(11, 0, 0, 1), net.IPv4(11, 0, 0, 2)
	for _, tc := range []struct {
		name string
		kind byte
		ip   net.IP
		ok   bool
	}{
		{"version 1", 0, nil, false},
		{"no address", ipNone, net.IPv4zero, false},
		{"another address", ipLocal, net.IPv4(11, 0, 0, 9), false},
		{"its address", ipLocal, ipA, true},
		{"our address", ipRemote, ipB, true},
	} {
		a, b := newEnd(t), newEnd(t)
		id := b.st.NewIdent(hello(ha, 3), ipA, func() net.IP { return ipB }, func(byte, []byte) error { return nil })
		id.Start()
		pub := a.st.PublicKey()
		id.Handle(&proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpPublicKey, Payload: append([]byte{byte(len(pub))}, pub...)})
		id.Handle(&proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpSignature, Payload: sigFrom(a, b.st, id.challenge, tc.kind, tc.ip)})
		if got := b.st.Identified(ha, ipA); got != tc.ok {
			t.Errorf("%s: identified %v", tc.name, got)
		}
	}
}

// A proof of identity lasts identTTL; a failed proof from the same
// address voids it at once (another computer behind it).
func TestIdentExpiresAndFails(t *testing.T) {
	ha, hb := ed2k.Hash{1}, ed2k.Hash{2}
	ipA, ipB := net.IPv4(11, 0, 0, 1), net.IPv4(11, 0, 0, 2)
	a, b := newEnd(t), newEnd(t)
	now := time.Now()
	b.st.now = func() time.Time { return now }
	connect(t, a, b, ha, hb, 3, 3, ipA, ipB)
	if !b.st.Identified(ha, ipA) {
		t.Fatal("not identified")
	}
	now = now.Add(identTTL + time.Minute)
	if b.st.Identified(ha, ipA) || b.st.Ratio(ha, ipA) != 1 {
		t.Fatal("proof outlived identTTL")
	}
	// Asked again on the next connection: proven again.
	connect(t, &end{st: a.st}, &end{st: b.st}, ha, hb, 3, 3, ipA, ipB)
	if !b.st.Identified(ha, ipA) {
		t.Fatal("not proven again")
	}
	// Another computer at the same address, same hash, other key: fails,
	// and the proof from that address is void.
	b.st.Downloaded(ha, ipA, 5<<20)
	x := newEnd(t)
	connect(t, x, &end{st: b.st}, ha, hb, 3, 3, ipA, ipB)
	if b.st.Identified(ha, ipA) {
		t.Fatal("a failed proof left the old one standing")
	}
}

// A hash that earned no credits yet is not held by the first key that
// proved it: its owner can prove it with its own. One with credits is.
func TestIdentKeyOfHashWithoutCredits(t *testing.T) {
	ha, hb := ed2k.Hash{1}, ed2k.Hash{2}
	ipA, ipB, ipX := net.IPv4(11, 0, 0, 1), net.IPv4(11, 0, 0, 2), net.IPv4(11, 0, 0, 3)
	squatter, owner, b := newEnd(t), newEnd(t), newEnd(t)
	connect(t, squatter, b, ha, hb, 3, 3, ipX, ipB)
	if !b.st.Identified(ha, ipX) {
		t.Fatal("first proof refused")
	}
	connect(t, owner, &end{st: b.st}, ha, hb, 3, 3, ipA, ipB)
	if !b.st.Identified(ha, ipA) || !bytes.Equal(b.st.KeyOf(ha), owner.st.PublicKey()) {
		t.Fatal("the owner could not take back a hash with no credits")
	}
	b.st.Downloaded(ha, ipA, 5<<20)
	connect(t, &end{st: squatter.st}, &end{st: b.st}, ha, hb, 3, 3, ipX, ipB)
	if b.st.Identified(ha, ipX) || !bytes.Equal(b.st.KeyOf(ha), owner.st.PublicKey()) {
		t.Fatal("a hash with credits taken over by another key")
	}
}

// Made-up identities (which cost nothing) never push out credits, and are
// not saved.
func TestStoreKeepsEarnedCredits(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir, t.Logf)
	ip := net.IPv4(11, 0, 0, 1)
	honest := ed2k.Hash{0xAA}
	st.verified(honest, ip, []byte("k"))
	st.Downloaded(honest, ip, 3<<20)
	st.recs[honest].seen = time.Now().Add(-100 * 24 * time.Hour).Unix() // seen long ago
	for i := range maxRecords + 50 {
		var h ed2k.Hash
		h[0], h[1], h[2], h[3] = 1, byte(i), byte(i>>8), byte(i>>16)
		st.verified(h, ip, []byte("k"))
	}
	if st.Len() != maxRecords {
		t.Fatal(st.Len())
	}
	if c, ok := st.Get(honest, ip); !ok || c.Downloaded != 3<<20 {
		t.Fatal("credits pushed out by made-up identities")
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2, _ := Open(dir, t.Logf)
	if st2.Len() != 1 {
		t.Fatalf("%d records saved, want only the one with credits", st2.Len())
	}
}

// A peer that does not know its own public address (LowID) signs over
// ours as it sees it, and is identified.
func TestIdentSignerWithoutItsAddress(t *testing.T) {
	ha := ed2k.Hash{1}
	ipA, ipB := net.IPv4(11, 0, 0, 1), net.IPv4(11, 0, 0, 2)
	a, b := newEnd(t), newEnd(t)
	var toA, toB []*proto.Packet
	q := func(l *[]*proto.Packet) func(byte, []byte) error {
		return func(op byte, p []byte) error {
			*l = append(*l, &proto.Packet{Proto: proto.ProtoEMule, Opcode: op, Payload: p})
			return nil
		}
	}
	ida := a.st.NewIdent(hello(ed2k.Hash{2}, 3), ipB, nil, q(&toB)) // a does not know its address
	idb := b.st.NewIdent(hello(ha, 3), ipA, func() net.IP { return ipB }, q(&toA))
	ida.Start()
	idb.Start()
	for range 6 {
		x, y := toA, toB
		toA, toB = nil, nil
		for _, p := range x {
			ida.Handle(p)
		}
		for _, p := range y {
			idb.Handle(p)
		}
	}
	if !b.st.Identified(ha, ipA) {
		t.Fatal("not identified by a signature over our address")
	}
}
