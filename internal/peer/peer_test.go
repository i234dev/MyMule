package peer

import (
	"net"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

func TestHelloRoundTrip(t *testing.T) {
	var uh ed2k.Hash
	uh[0] = 9
	h := NewHello(uh, "tester", 1234, 4662, net.IPv4(1, 2, 3, 4), 4661)
	for _, answer := range []bool{false, true} {
		got, err := DecodeHello(h.Encode(answer), answer)
		if err != nil {
			t.Fatal(err)
		}
		if got.UserHash != uh || got.ClientID != 1234 || got.Port != 4662 ||
			got.Name() != "tester" || got.ServerIP.String() != "1.2.3.4" || got.ServerPort != 4661 {
			t.Fatalf("answer=%v: %+v", answer, got)
		}
	}
}

func TestFileStatus(t *testing.T) {
	var w proto.Writer
	w.Bytes(make([]byte, 16))
	w.U16(10)
	w.Bytes([]byte{0b00000101, 0b00000010})
	_, avail, err := DecodeFileStatus(w.B)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, false, true, false, false, false, false, false, false, true}
	for i := range want {
		if avail[i] != want[i] {
			t.Fatalf("avail = %v", avail)
		}
	}
}

func TestDataPart(t *testing.T) {
	var w proto.Writer
	w.Bytes(make([]byte, 16))
	w.U32(100)
	w.U32(103)
	w.Bytes([]byte("abc"))
	d, err := DecodeDataPart(&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: OpSendingPart, Payload: w.B})
	if err != nil || d.Start != 100 || d.End != 103 || string(d.Data) != "abc" {
		t.Fatalf("%+v %v", d, err)
	}
}

// A hello whose tag count is 0xFFFFFFFF used to crash the whole daemon on
// 32-bit builds (int(0xFFFFFFFF) is -1 there and make() panics). It must be
// an ordinary decode error on every architecture.
func TestDecodeHelloHugeTagCount(t *testing.T) {
	var w proto.Writer
	w.U8(16)
	w.Bytes(make([]byte, 16)) // user hash
	w.U32(0)                  // client ID
	w.U16(0)                  // port
	w.U32(0xFFFFFFFF)         // tag count
	w.Bytes(make([]byte, 8))
	if _, err := DecodeHello(w.B, false); err == nil {
		t.Fatal("DecodeHello with tag count 0xFFFFFFFF: want error, got nil")
	}
}

func TestHelloNameIsCleaned(t *testing.T) {
	h := Hello{Tags: proto.TagList{proto.StrTag(CTName, "nick\x1b[31m\u202eevil\nline")}}
	if got := h.Name(); got != "nick[31mevil line" {
		t.Fatalf("Name() = %q", got)
	}
}
