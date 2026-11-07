package kad

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"

	"mymule/internal/proto"
)

// Kad is IPv4 only: other addresses never panic.
func TestCryptNotIPv4(t *testing.T) {
	k := testKeys(1)
	d := append([]byte{0x10}, make([]byte, 20)...)
	for _, ip := range []netip.Addr{{}, netip.MustParseAddr("2001:db8::1")} {
		if _, err := k.Decrypt(d, ip); err == nil {
			t.Errorf("%v: decrypted", ip)
		}
		if k.VerifyKey(ip) != 0 {
			t.Errorf("%v: verify key", ip)
		}
		plain := []byte{ProtoKad, OpPing}
		if !bytes.Equal(k.EncryptKad(plain, ip, k.KadID, 5), plain) {
			t.Errorf("%v: EncryptKad encrypted", ip)
		}
		if !bytes.Equal(k.EncryptClient(plain, k.UserHash, ip), plain) {
			t.Errorf("%v: EncryptClient encrypted", ip)
		}
	}
}

// handMade builds an obfuscated Kad datagram the way eMule's
// EncryptSendClient does, independently of EncryptKad, with padding.
func handMade(key []byte, markerBits byte, pad int, recvKey, sendKey uint32, data []byte) []byte {
	random := []byte{0x5A, 0xA5}
	h := md5.New()
	h.Write(key)
	h.Write(random)
	c, _ := rc4.NewCipher(h.Sum(nil))
	var plain []byte
	plain = binary.LittleEndian.AppendUint32(plain, magicSyncClient)
	plain = append(plain, byte(pad))
	plain = append(plain, bytes.Repeat([]byte{0x77}, pad)...)
	plain = binary.LittleEndian.AppendUint32(plain, recvKey)
	plain = binary.LittleEndian.AppendUint32(plain, sendKey)
	plain = append(plain, data...)
	enc := make([]byte, len(plain))
	c.XORKeyStream(enc, plain)
	return append([]byte{0x40 | markerBits, random[0], random[1]}, enc...)
}

// A datagram keyed with the verify key we gave the sender, with padding,
// laid out by hand: [marker][random][magic][pad len][pad][receiver key]
// [sender key][packet].
func TestCryptKnownAnswer(t *testing.T) {
	me := testKeys(2)
	data := []byte{ProtoKad, OpHelloRes, 1, 2, 3}
	ours := me.VerifyKey(ipA)
	var vk [4]byte
	binary.LittleEndian.PutUint32(vk[:], ours)
	for _, pad := range []int{0, 1, 7} {
		d := handMade(vk[:], 2, pad, ours, 0xCAFEBABE, data)
		r, err := me.Decrypt(d, ipA)
		if err != nil || !bytes.Equal(r.Data, data) || !r.ValidKey || r.SenderKey != 0xCAFEBABE {
			t.Fatalf("pad %d: %v %+v", pad, err, r)
		}
	}
	// Keyed with our ID, padded.
	w := me.KadID.Wire()
	r, err := me.Decrypt(handMade(w[:], 0, 3, 0, 9, data), ipA)
	if err != nil || !bytes.Equal(r.Data, data) || r.ValidKey || r.SenderKey != 9 {
		t.Fatalf("by ID: %v %+v", err, r)
	}
	// What EncryptKad sends is laid out the same way.
	enc := testKeys(1).EncryptKad(data, ipB, ID{}, ours)
	random := enc[1:3]
	h := md5.New()
	h.Write(vk[:])
	h.Write(random)
	c, _ := rc4.NewCipher(h.Sum(nil))
	plain := make([]byte, len(enc)-3)
	c.XORKeyStream(plain, enc[3:])
	if binary.LittleEndian.Uint32(plain) != magicSyncClient || plain[4] != 0 ||
		binary.LittleEndian.Uint32(plain[5:]) != ours ||
		binary.LittleEndian.Uint32(plain[9:]) != testKeys(1).VerifyKey(ipB) || !bytes.Equal(plain[13:], data) {
		t.Fatalf("EncryptKad layout: % X", plain)
	}
}

// Old clients set the marker bits at random: every key is tried whatever
// they say.
func TestCryptMisleadingMarker(t *testing.T) {
	me := testKeys(2)
	data := []byte{ProtoKad, OpPing, 0}
	w := me.KadID.Wire()
	var vk [4]byte
	binary.LittleEndian.PutUint32(vk[:], me.VerifyKey(ipA))
	for bits := byte(0); bits < 4; bits++ {
		for name, key := range map[string][]byte{"id": w[:], "verify key": vk[:]} {
			r, err := me.Decrypt(handMade(key, bits, 0, 0, 1, data), ipA)
			if err != nil || !bytes.Equal(r.Data, data) {
				t.Errorf("%s with marker bits %d: %v", name, bits, err)
			}
		}
		r, err := me.Decrypt(testKeys(1).EncryptClient(data, me.UserHash, ipA), ipA)
		if err != nil || !bytes.Equal(r.Data, data) {
			t.Errorf("client: %v", err)
		}
	}
}

// A compressed payload may expand to exactly the limit, not one byte more.
func TestPacketUnpackBoundary(t *testing.T) {
	prefix := make([]byte, 300)
	rand.Read(prefix)
	exact, over := false, false
	for z := 2000; z < 6000 && !(exact && over); z++ {
		payload := append(append([]byte(nil), prefix...), make([]byte, z)...)
		var buf bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
		zw.Write(payload)
		zw.Close()
		d := append([]byte{ProtoKadPacked, 1}, buf.Bytes()...)
		limit := unpackLimit(len(d))
		_, err := DecodePacket(d)
		switch {
		case len(payload) <= limit && err != nil:
			t.Fatalf("%d bytes, limit %d: %v", len(payload), limit, err)
		case len(payload) > limit && err == nil:
			t.Fatalf("%d bytes, limit %d: accepted", len(payload), limit)
		}
		exact = exact || len(payload) == limit
		over = over || len(payload) == limit+1
		// Encode packs exactly what eMule can unpack.
		if e := (Packet{Opcode: 1, Payload: payload}).Encode(); (e[0] == ProtoKadPacked) != (len(payload) <= unpackLimit(len(e))) && len(buf.Bytes())+2 == len(e) {
			t.Fatalf("Encode packed %v for %d bytes", e[0] == ProtoKadPacked, len(payload))
		}
	}
	if !exact || !over {
		t.Fatalf("boundary not reached: exact %v, over %v", exact, over)
	}
}

func TestTagTextAndTruncation(t *testing.T) {
	b := []byte{1, proto.TagString, 1, 0, TagFileName, 6, 0, 0xEF, 0xBB, 0xBF, 'a', 'b', 'c'}
	tags, err := ReadTags(proto.NewReader(b))
	if err != nil || TagStr(tags, TagFileName) != "abc" {
		t.Fatalf("BOM: %v %q", err, TagStr(tags, TagFileName))
	}
	long := strings.Repeat("é", 40000) // 80000 bytes
	var w proto.Writer
	WriteTags(&w, []proto.Tag{StrTag(TagFileName, long)})
	// Read back without ReadTags' length limit.
	r := proto.NewReader(w.B[1+4:])
	s := r.Str16()
	if r.Err() != nil || !utf8.ValidString(s) || len(s) < 65534 || !strings.HasPrefix(long, s) {
		t.Fatalf("truncated to %d bytes, valid %v", len(s), utf8.ValidString(s))
	}
	// The buddy hash is a string tag in eMule.
	WriteTags(&w, []proto.Tag{StrTag(TagBuddyHash, "0123456789ABCDEF0123456789ABCDEF")})
}

func FuzzDecrypt(f *testing.F) {
	k := testKeys(1)
	f.Add(k.EncryptKad([]byte{ProtoKad, OpPing}, ipA, k.KadID, 0), []byte{10, 1, 2, 3})
	f.Add([]byte{0x10, 1, 2, 3, 4, 5, 6, 7, 8, 9}, []byte{})
	f.Fuzz(func(t *testing.T, d, ip []byte) {
		addr, _ := netip.AddrFromSlice(ip)
		if r, err := k.Decrypt(d, addr); err == nil {
			DecodePacket(r.Data)
		}
	})
}

func FuzzDecodePacket(f *testing.F) {
	f.Add(Packet{Opcode: 1, Payload: bytes.Repeat([]byte("Blue Harbor "), 40)}.Encode())
	f.Fuzz(func(t *testing.T, d []byte) {
		p, err := DecodePacket(d)
		if err == nil && len(p.Payload) > unpackLimit(len(d)) && len(d) > 0 && d[0] == ProtoKadPacked {
			t.Fatal("over the limit")
		}
	})
}

func FuzzReadTags(f *testing.F) {
	var w proto.Writer
	WriteTags(&w, []proto.Tag{StrTag(TagFileName, "Quiet Orchard"), IntTag(TagFileSize, 1<<33)})
	f.Add(w.B)
	f.Fuzz(func(t *testing.T, d []byte) {
		tags, err := ReadTags(proto.NewReader(d))
		if err == nil {
			var w proto.Writer
			WriteTags(&w, tags)
		}
	})
}
