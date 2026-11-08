package kad

import (
	"bytes"
	"compress/zlib"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"testing"

	"mymule/internal/proto"
)

// IDs go on the wire as four little-endian 32-bit numbers, most
// significant first.
func TestIDWire(t *testing.T) {
	id, ok := ParseID("00112233445566778899AABBCCDDEEFF")
	if !ok {
		t.Fatal("ParseID failed")
	}
	want := []byte{0x33, 0x22, 0x11, 0x00, 0x77, 0x66, 0x55, 0x44, 0xBB, 0xAA, 0x99, 0x88, 0xFF, 0xEE, 0xDD, 0xCC}
	w := id.Wire()
	if !bytes.Equal(w[:], want) {
		t.Fatalf("Wire = % X, want % X", w, want)
	}
	if IDFromWire(w) != id {
		t.Fatal("IDFromWire(Wire) changed the ID")
	}
	var pw proto.Writer
	WriteID(&pw, id)
	if got := ReadID(proto.NewReader(pw.B)); got != id {
		t.Fatalf("ReadID = %s", got)
	}
	if id.String() != "00112233445566778899AABBCCDDEEFF" {
		t.Fatalf("String = %s", id)
	}
	if id.Chunk(0) != 0x00112233 || id.Chunk(3) != 0xCCDDEEFF {
		t.Fatalf("Chunk = %08X %08X", id.Chunk(0), id.Chunk(3))
	}
}

func TestIDBitsAndDistance(t *testing.T) {
	var a ID
	a.SetBit(0, 1)
	a.SetBit(127, 1)
	if a[0] != 0x80 || a[15] != 0x01 || a.Bit(0) != 1 || a.Bit(1) != 0 || a.Bit(127) != 1 {
		t.Fatalf("bits wrong: %s", a)
	}
	a.SetBit(0, 0)
	if a.Bit(0) != 0 {
		t.Fatal("SetBit 0 did not clear")
	}
	x, _ := ParseID("F0000000000000000000000000000001")
	y, _ := ParseID("0F000000000000000000000000000003")
	if d := x.Xor(y); d.String() != "FF000000000000000000000000000002" {
		t.Fatalf("Xor = %s", d)
	}
	if x.Cmp(y) != 1 || y.Cmp(x) != -1 || x.Cmp(x) != 0 {
		t.Fatal("Cmp wrong")
	}
	for n := 0; n <= 128; n += 7 {
		r := x.WithPrefix(n)
		for i := 0; i < n && i < 128; i++ {
			if r.Bit(i) != x.Bit(i) {
				t.Fatalf("WithPrefix(%d) bit %d differs", n, i)
			}
		}
	}
}

func TestPacketSmallStaysPlain(t *testing.T) {
	b := Packet{Opcode: OpHelloReq, Payload: bytes.Repeat([]byte{'a'}, 200)}.Encode()
	if b[0] != ProtoKad || b[1] != OpHelloReq || len(b) != 202 {
		t.Fatalf("got % X...", b[:2])
	}
	p, err := DecodePacket(b)
	if err != nil || p.Opcode != OpHelloReq || len(p.Payload) != 200 {
		t.Fatalf("DecodePacket: %v %+v", err, p)
	}
}

func TestPacketBigIsCompressed(t *testing.T) {
	var payload []byte
	for i := range 40 {
		payload = append(payload, fmt.Sprintf("Invented Film %d, Bright Comets %d. ", i*37, i*i)...)
	}
	b := Packet{Opcode: OpSearchRes, Payload: payload}.Encode()
	if b[0] != ProtoKadPacked || len(b) >= len(payload) {
		t.Fatalf("not compressed: proto %X, %d bytes", b[0], len(b))
	}
	p, err := DecodePacket(b)
	if err != nil || p.Opcode != OpSearchRes || !bytes.Equal(p.Payload, payload) {
		t.Fatalf("DecodePacket: %v", err)
	}
	// Random bytes do not compress: sent plain.
	rnd := make([]byte, 300)
	rand.Read(rnd)
	if b := (Packet{Opcode: 1, Payload: rnd}).Encode(); b[0] != ProtoKad {
		t.Fatalf("incompressible payload sent as %X", b[0])
	}
	// Nor is a payload that compresses so well eMule would refuse to
	// unpack it.
	if b := (Packet{Opcode: 1, Payload: make([]byte, 5000)}).Encode(); b[0] != ProtoKad {
		t.Fatalf("over-compressible payload sent as %X", b[0])
	}
}

// A small compressed packet may not expand to more than eMule accepts
// (10 times its size plus 300 bytes).
func TestPacketBombRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(make([]byte, 1<<20))
	zw.Close()
	b := append([]byte{ProtoKadPacked, OpSearchRes}, buf.Bytes()...)
	if _, err := DecodePacket(b); err == nil {
		t.Fatal("zlib bomb accepted")
	}
	if _, err := DecodePacket([]byte{ProtoKadPacked, 1, 1, 2, 3}); err == nil {
		t.Fatal("bad zlib accepted")
	}
	if _, err := DecodePacket([]byte{ProtoKad}); err == nil {
		t.Fatal("1-byte packet accepted")
	}
	if _, err := DecodePacket([]byte{0xE3, 1, 2}); err != ErrNotKad {
		t.Fatalf("eD2K packet: %v", err)
	}
}

var (
	ipA = netip.MustParseAddr("10.1.2.3")
	ipB = netip.MustParseAddr("10.9.8.7")
)

func testKeys(seed byte) *Keys {
	k := &Keys{}
	for i := range k.Secret {
		k.Secret[i] = seed ^ byte(i)
	}
	for i := range k.KadID {
		k.KadID[i] = seed + byte(i)
		k.UserHash[i] = seed*3 + byte(i)
	}
	return k
}

func TestVerifyKey(t *testing.T) {
	k := testKeys(1)
	a, b := k.VerifyKey(ipA), k.VerifyKey(ipB)
	if a == 0 || b == 0 || a == b {
		t.Fatalf("keys %08X %08X", a, b)
	}
	if k.VerifyKey(ipA) != a {
		t.Fatal("not deterministic")
	}
	if k.VerifyKey(netip.AddrFrom16(ipA.As16())) != a {
		t.Fatal("IPv4-mapped address gives another key")
	}
	other := testKeys(2)
	if other.VerifyKey(ipA) == a {
		t.Fatal("another secret gives the same key")
	}
	// HMAC-SHA256 of the IP's bytes with the secret, its first 32 bits
	// (little-endian) mod 0xFFFFFFFE, plus 1.
	m := hmac.New(sha256.New, k.Secret[:])
	m.Write([]byte{10, 1, 2, 3})
	want := binary.LittleEndian.Uint32(m.Sum(nil))%0xFFFFFFFE + 1
	if a != want {
		t.Fatalf("VerifyKey = %08X, want %08X", a, want)
	}
	// Every bit of the secret counts.
	flipped := *k
	flipped.Secret[31] ^= 1
	if flipped.VerifyKey(ipA) == a {
		t.Fatal("the end of the secret does not count")
	}
	if k.VerifyKey(netip.MustParseAddr("2001:db8::1")) != 0 {
		t.Fatal("IPv6 address got a key")
	}
}

// A node sending to us with our Kad ID as the key (its first packet).
func TestCryptKadByID(t *testing.T) {
	sender, me := testKeys(1), testKeys(2)
	plain := Packet{Opcode: OpHelloReq, Payload: []byte("hello there")}.Encode()
	enc := sender.EncryptKad(plain, ipB, me.KadID, 0)
	if bytes.Contains(enc, []byte("hello")) || isPlainMarker(enc[0]) || enc[0]&3 != 0 {
		t.Fatalf("not obfuscated, marker %02X", enc[0])
	}
	r, err := me.Decrypt(enc, ipA)
	if err != nil || !bytes.Equal(r.Data, plain) || !r.Encrypted {
		t.Fatalf("Decrypt: %v % X", err, r.Data)
	}
	if r.ValidKey {
		t.Fatal("receiver key 0 counted as valid")
	}
	if r.SenderKey != sender.VerifyKey(ipB) {
		t.Fatalf("sender key %08X", r.SenderKey)
	}
}

// Answering with the verify key the other node gave us.
func TestCryptKadByVerifyKey(t *testing.T) {
	me, other := testKeys(1), testKeys(2)
	theirKeyForMe := other.VerifyKey(ipA)
	plain := Packet{Opcode: OpHelloRes, Payload: []byte("answer")}.Encode()
	enc := me.EncryptKad(plain, ipB, ID{}, theirKeyForMe)
	if enc[0]&3 != 2 {
		t.Fatalf("marker %02X", enc[0])
	}
	r, err := other.Decrypt(enc, ipA)
	if err != nil || !bytes.Equal(r.Data, plain) {
		t.Fatalf("Decrypt: %v", err)
	}
	if !r.ValidKey {
		t.Fatal("our own key not recognised")
	}
	if r.SenderKey != me.VerifyKey(ipB) {
		t.Fatal("sender key lost")
	}
	// The same datagram claiming to come from another IP: the key no
	// longer matches (a forged source address).
	if r, err := other.Decrypt(enc, netip.MustParseAddr("10.5.5.5")); err == nil && r.ValidKey {
		t.Fatal("key accepted from another IP")
	}
	// Neither ID nor key known: sent plain.
	if got := me.EncryptKad(plain, ipB, ID{}, 0); !bytes.Equal(got, plain) {
		t.Fatal("encrypted without a key")
	}
}

func TestCryptClient(t *testing.T) {
	sender, me := testKeys(1), testKeys(2)
	plain := []byte{ProtoEMule, 0x90, 1, 2, 3}
	enc := sender.EncryptClient(plain, me.UserHash, ipA)
	if enc[0]&1 != 1 || isPlainMarker(enc[0]) {
		t.Fatalf("marker %02X", enc[0])
	}
	r, err := me.Decrypt(enc, ipA)
	if err != nil || !bytes.Equal(r.Data, plain) || r.ValidKey || r.SenderKey != 0 {
		t.Fatalf("Decrypt: %v %+v", err, r)
	}
	if _, err := me.Decrypt(enc, ipB); err == nil {
		t.Fatal("decrypted with the wrong sender IP")
	}
}

func TestCryptPlainAndJunk(t *testing.T) {
	me := testKeys(1)
	for _, b := range plainMarkers {
		d := []byte{b, 1, 2, 3, 4, 5, 6, 7, 8, 9}
		r, err := me.Decrypt(d, ipA)
		if err != nil || r.Encrypted || !bytes.Equal(r.Data, d) {
			t.Fatalf("plain %02X: %v", b, err)
		}
	}
	junk := []byte{0x10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if _, err := me.Decrypt(junk, ipA); err != ErrBadCrypt {
		t.Fatalf("junk: %v", err)
	}
	if _, err := me.Decrypt(nil, ipA); err == nil {
		t.Fatal("empty datagram accepted")
	}
	// A packet for someone else's ID cannot be read.
	other := testKeys(3)
	enc := other.EncryptKad([]byte{ProtoKad, OpPing}, ipA, testKeys(4).KadID, 0)
	if _, err := me.Decrypt(enc, ipB); err == nil {
		t.Fatal("decrypted a packet for another node")
	}
}

// Datagrams too short to hold their padding or verify keys are refused,
// not read past their end.
func TestCryptTruncated(t *testing.T) {
	sender, me := testKeys(1), testKeys(2)
	enc := sender.EncryptKad([]byte{ProtoKad, OpPing}, ipB, me.KadID, 0)
	for n := 1; n <= cryptHeaderLen+8; n++ {
		r, err := me.Decrypt(enc[:n], ipA)
		if err == nil && r.Encrypted {
			t.Fatalf("truncated to %d bytes: decrypted", n)
		}
	}
	// A header announcing more padding than there is.
	var random = []byte{0x42, 0x42}
	w := me.KadID.Wire()
	h := md5.New()
	h.Write(w[:])
	h.Write(random)
	c, _ := rc4.NewCipher(h.Sum(nil))
	hdr := make([]byte, 5)
	binary.LittleEndian.PutUint32(hdr, magicSyncClient)
	hdr[4] = 200
	c.XORKeyStream(hdr, hdr)
	d := append([]byte{0x40, random[0], random[1]}, hdr...)
	d = append(d, make([]byte, 50)...)
	if _, err := me.Decrypt(d, ipA); err == nil {
		t.Fatal("padding past the end accepted")
	}
}

// A client datagram whose padding takes all of it holds no packet.
func TestCryptAllPadding(t *testing.T) {
	me := testKeys(2)
	random := []byte{0x42, 0x42}
	a := ipA.As4()
	h := md5.New()
	h.Write(me.UserHash[:])
	h.Write(a[:])
	h.Write([]byte{magicUDP})
	h.Write(random)
	c, _ := rc4.NewCipher(h.Sum(nil))
	hdr := make([]byte, 5)
	binary.LittleEndian.PutUint32(hdr, magicSyncClient)
	hdr[4] = 6
	c.XORKeyStream(hdr, hdr)
	d := append([]byte{0x41, random[0], random[1]}, hdr...)
	d = append(d, make([]byte, 6)...)
	if r, err := me.Decrypt(d, ipA); err == nil {
		t.Fatalf("all-padding datagram read as % X", r.Data)
	}
}

func TestMarkerNeverPlain(t *testing.T) {
	k := testKeys(1)
	for i := 0; i < 2000; i++ {
		if b := k.EncryptKad([]byte{ProtoKad, OpPing}, ipA, k.KadID, 0); isPlainMarker(b[0]) || b[0]&3 != 0 {
			t.Fatalf("Kad marker %02X", b[0])
		}
		if b := k.EncryptClient([]byte{ProtoEMule, 1}, k.UserHash, ipA); isPlainMarker(b[0]) || b[0]&1 != 1 {
			t.Fatalf("client marker %02X", b[0])
		}
	}
}

func TestTags(t *testing.T) {
	tags := []proto.Tag{
		StrTag(TagFileName, "Quiet Orchard.mkv"),
		IntTag(TagFileSize, 200),
		IntTag(TagSources, 70000),
		IntTag(TagFileSizeHi, 1<<40),
		IntTag(TagSourcePort, 4662),
		{Name: "long name", Type: proto.TagFloat32, Float: 1.5},
		{ID: 0x98, Type: proto.TagHash16, Bytes: bytes.Repeat([]byte{7}, 16)},
		{ID: 0x99, Type: proto.TagBsob, Bytes: []byte{1, 2, 3}},
	}
	var w proto.Writer
	WriteTags(&w, tags)
	// Integers use the smallest type, as eMule does.
	if w.B[1+4+2+len("Quiet Orchard.mkv")] != proto.TagUint8 {
		t.Fatalf("size 200 not written as uint8")
	}
	got, err := ReadTags(proto.NewReader(w.B))
	if err != nil || len(got) != len(tags) {
		t.Fatalf("ReadTags: %v, %d tags", err, len(got))
	}
	if TagStr(got, TagFileName) != "Quiet Orchard.mkv" {
		t.Fatal("name lost")
	}
	for id, want := range map[byte]uint64{TagFileSize: 200, TagSources: 70000, TagFileSizeHi: 1 << 40, TagSourcePort: 4662} {
		if v, ok := TagInt(got, id); !ok || v != want {
			t.Fatalf("tag %02X = %d %v", id, v, ok)
		}
	}
	if got[5].Name != "long name" || got[5].Float != 1.5 || !bytes.Equal(got[6].Bytes, tags[6].Bytes) || !bytes.Equal(got[7].Bytes, []byte{1, 2, 3}) {
		t.Fatalf("other tags: %+v", got[5:])
	}
}

func TestTagsBad(t *testing.T) {
	for name, b := range map[string][]byte{
		"count too big":   {50, 2, 1, 0, 1, 0, 0},
		"unknown type":    {1, 0x42, 1, 0, 1, 0},
		"compact form":    {1, 0x82, 1, 1, 'a'},
		"truncated":       {1, proto.TagString, 1, 0, 1, 10, 0, 'a'},
		"huge string":     append([]byte{1, proto.TagString, 1, 0, 1, 0x88, 0x13}, make([]byte, 5000)...),
		"huge name":       append([]byte{1, proto.TagUint8, 0x88, 0x13}, make([]byte, 5001)...),
		"no count at all": {},
	} {
		if _, err := ReadTags(proto.NewReader(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Latin-1 strings become UTF-8.
	b := []byte{1, proto.TagString, 1, 0, TagFileName, 3, 0, 'c', 0xE9, 'u'}
	tags, err := ReadTags(proto.NewReader(b))
	if err != nil || TagStr(tags, TagFileName) != "céu" {
		t.Fatalf("Latin-1: %v %q", err, TagStr(tags, TagFileName))
	}
}
