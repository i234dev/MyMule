package proto

import (
	"bytes"
	"compress/zlib"
	"testing"
)

func TestPacketRoundTrip(t *testing.T) {
	p := &Packet{Proto: ProtoEDonkey, Opcode: OpLoginRequest, Payload: []byte{1, 2, 3}}
	enc := p.Encode()
	want := []byte{0xE3, 4, 0, 0, 0, 0x01, 1, 2, 3}
	if !bytes.Equal(enc, want) {
		t.Fatalf("encode = % x", enc)
	}
	got, err := ReadPacket(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	if got.Opcode != p.Opcode || !bytes.Equal(got.Payload, p.Payload) {
		t.Fatalf("decoded %+v", got)
	}
}

func TestPackedPacket(t *testing.T) {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write([]byte("hello hello hello"))
	zw.Close()
	p := &Packet{Proto: ProtoPacked, Opcode: OpSearchResult, Payload: z.Bytes()}
	got, err := ReadPacket(bytes.NewReader(p.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != "hello hello hello" || got.Proto != ProtoEMule {
		t.Fatalf("got %+v", got)
	}
}

func TestTagRoundTrip(t *testing.T) {
	var w Writer
	w.WriteTag(StrTag(CTName, "mymule"))
	w.WriteTag(IntTag(CTVersion, 0x3C))
	w.WriteTag(Tag{Name: "users", Type: TagUint32, Int: 42})
	r := NewReader(w.B)
	tags, err := r.ReadTags(3)
	if err != nil {
		t.Fatal(err)
	}
	l := TagList(tags)
	if l.Str(CTName) != "mymule" || l.Int(CTVersion) != 0x3C {
		t.Fatalf("tags %v", tags)
	}
	if u, ok := l.FindName("users"); !ok || u.Int != 42 {
		t.Fatalf("named tag %v", u)
	}
}

func TestCompactTags(t *testing.T) {
	// Compact short string: type 0x80|0x13 (3-char string), id 0x01, "abc".
	// Compact uint8: type 0x80|0x09, id 0x02, value 7.
	b := []byte{0x93, 0x01, 'a', 'b', 'c', 0x89, 0x02, 7}
	tags, err := NewReader(b).ReadTags(2)
	if err != nil {
		t.Fatal(err)
	}
	l := TagList(tags)
	if l.Str(FTFileName) != "abc" || l.Int(FTFileSize) != 7 {
		t.Fatalf("tags %v", tags)
	}
}

// A tag count comes straight from the network. It must be rejected, not
// trusted, when it is negative (a u32 above 0x7FFFFFFF on 32-bit builds),
// absurdly large, or larger than the bytes left could possibly hold.
func TestReadTagsRejectsBadCount(t *testing.T) {
	var w Writer
	w.WriteTag(IntTag(0x01, 7))
	payload := w.B

	for _, n := range []int{-1, -0x80000000, MaxTags + 1, 1 << 30, 2} {
		r := NewReader(payload)
		if _, err := r.ReadTags(n); err == nil {
			t.Errorf("ReadTags(%d) on a one-tag payload: want error, got nil", n)
		}
	}

	r := NewReader(payload)
	tags, err := r.ReadTags(1)
	if err != nil || len(tags) != 1 || tags[0].Int != 7 {
		t.Fatalf("ReadTags(1) = %v, %v; want one tag with value 7", tags, err)
	}
}
