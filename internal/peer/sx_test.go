package peer

import (
	"bytes"
	"net"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

func sxSample() []SXSource {
	return []SXSource{
		{ID: ipToU32(net.IPv4(203, 0, 113, 0)), Port: 4662, ServerIP: net.IPv4(198, 51, 100, 7).To4(), ServerPort: 4661, UserHash: ed2k.Hash{1, 2}, Crypt: 3},
		{ID: ipToU32(net.IPv4(198, 51, 100, 20)), Port: 5000, ServerIP: net.IPv4zero.To4(), UserHash: ed2k.Hash{9}, Crypt: 1},
	}
}

// Every version round-trips the fields it carries; the others are zero.
func TestSXRoundTrip(t *testing.T) {
	file := ed2k.Hash{0xAB, 0xCD}
	for v := byte(1); v <= SXVersion; v++ {
		b := EncodeAnswerSources2(v, file, sxSample())
		gv, gf, got, err := DecodeAnswerSources2(b)
		if err != nil || gv != v || gf != file || len(got) != 2 {
			t.Fatalf("v%d: %v %d %v %d", v, err, gv, gf, len(got))
		}
		for i, want := range sxSample() {
			g := got[i]
			if g.ID != want.ID || g.Port != want.Port || !g.ServerIP.Equal(want.ServerIP) || g.ServerPort != want.ServerPort {
				t.Fatalf("v%d source %d: %+v", v, i, g)
			}
			if (v >= 2) != (g.UserHash == want.UserHash) || (v >= 4) != (g.Crypt == want.Crypt) {
				t.Fatalf("v%d source %d: hash %v crypt %d", v, i, g.UserHash, g.Crypt)
			}
		}
		f1, got1, err := DecodeAnswerSources(EncodeAnswerSources(v, file, sxSample()), v)
		if err != nil || f1 != file || len(got1) != 2 || got1[0].ID != sxSample()[0].ID {
			t.Fatalf("first form v%d: %v %+v", v, err, got1)
		}
	}
}

// From version 3 the ID goes in the opposite byte order (eMule's hybrid
// ID): an IP ending in .0 must not look like a LowID.
func TestSXHybridID(t *testing.T) {
	src := []SXSource{{ID: ipToU32(net.IPv4(203, 0, 113, 0)), Port: 1}}
	b3 := EncodeAnswerSources2(3, ed2k.Hash{}, src)
	b2 := EncodeAnswerSources2(2, ed2k.Hash{}, src)
	// version, hash, count, then the ID.
	if !bytes.Equal(b3[19:23], []byte{0, 113, 0, 203}) || !bytes.Equal(b2[19:23], []byte{203, 0, 113, 0}) {
		t.Fatalf("v3 % X, v2 % X", b3[19:23], b2[19:23])
	}
}

// Answers that do not add up are refused whole.
func TestSXRefused(t *testing.T) {
	good := EncodeAnswerSources2(4, ed2k.Hash{}, sxSample())
	for name, b := range map[string][]byte{
		"truncated":      good[:len(good)-1],
		"trailing bytes": append(append([]byte(nil), good...), 0),
		"version 0":      append([]byte{0}, good[1:]...),
		"version 5":      append([]byte{5}, good[1:]...),
		"no count":       good[:17],
	} {
		if _, _, _, err := DecodeAnswerSources2(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	var w proto.Writer
	w.U8(4)
	w.Bytes(make([]byte, 16))
	w.U16(MaxSXSources + 1)
	w.Bytes(make([]byte, (MaxSXSources+1)*sxEntrySize(4)))
	if _, _, _, err := DecodeAnswerSources2(w.B); err == nil {
		t.Error("more than MaxSXSources accepted")
	}
}

func TestSXRequest(t *testing.T) {
	file := ed2k.Hash{7, 7}
	p := &proto.Packet{Proto: proto.ProtoEMule, Opcode: OpRequestSources2, Payload: EncodeRequestSources2(file)}
	if f, v, ok := DecodeRequestSources(p, 0); !ok || f != file || v != SXVersion {
		t.Fatalf("second form: %v %d %v", f, v, ok)
	}
	p1 := &proto.Packet{Proto: proto.ProtoEMule, Opcode: OpRequestSources, Payload: file[:]}
	if f, v, ok := DecodeRequestSources(p1, 3); !ok || f != file || v != 3 {
		t.Fatalf("first form: %v %d %v", f, v, ok)
	}
	if _, _, ok := DecodeRequestSources(p1, 0); ok {
		t.Fatal("first form from a peer without it")
	}
	big := &proto.Packet{Proto: proto.ProtoEMule, Opcode: OpRequestSources2, Payload: append([]byte{9, 0, 0}, file[:]...)}
	if _, v, ok := DecodeRequestSources(big, 0); !ok || v != SXVersion {
		t.Fatalf("a later version is answered with ours: %d %v", v, ok)
	}
	if _, _, ok := DecodeRequestSources(&proto.Packet{Opcode: OpRequestSources2, Payload: []byte{4, 0}}, 0); ok {
		t.Fatal("truncated request accepted")
	}
}

func TestSXHelloFlags(t *testing.T) {
	h := Hello{Tags: proto.TagList{proto.IntTag(CTEMuleMiscOpts1, 4<<12), proto.IntTag(CTEMuleMiscOpts2, 1<<10)}}
	if h.SourceExchange1() != 4 || !h.SourceExchange2() || !h.SourceExchange() {
		t.Fatal("flags not read")
	}
	old := Hello{Tags: proto.TagList{proto.IntTag(CTEMuleMiscOpts1, 1<<12)}}
	if old.SourceExchange() {
		t.Fatal("version 1 only counts as source exchange (eMule asks above 1)")
	}
}

// Our hello says we do source exchange, both forms.
func TestOurHelloOffersSX(t *testing.T) {
	h := NewHello(ed2k.Hash{1}, "me", 0x02000000, 4662, nil, 0).WithKad(4672, 8).WithCrypt(1)
	if h.SourceExchange1() != SXVersion || !h.SourceExchange2() {
		t.Fatalf("version %d, second form %v", h.SourceExchange1(), h.SourceExchange2())
	}
}

// eMule's wire bytes: a LowID is sent as it is, from version 3 too, and
// stays a LowID; a HighID comes byte-reversed.
func TestSXDecodeHybridLowID(t *testing.T) {
	var w proto.Writer
	w.U8(4)
	w.Bytes(make([]byte, 16))
	w.U16(2)
	for _, id := range []uint32{0x00123456, swap32(ipToU32(net.IPv4(1, 2, 3, 4)))} {
		w.U32(id)
		w.U16(4662)
		w.U32(0)
		w.U16(0)
		w.Bytes(make([]byte, 16))
		w.U8(0)
	}
	_, _, srcs, err := DecodeAnswerSources2(w.B)
	if err != nil || srcs[0].ID != 0x00123456 || srcs[1].ID != ipToU32(net.IPv4(1, 2, 3, 4)) {
		t.Fatalf("%v %+v", err, srcs)
	}
}

// eMule may send 501 sources; more is refused.
func TestSXMaxSources(t *testing.T) {
	src := make([]SXSource, 501)
	for i := range src {
		src[i] = SXSource{ID: 1 << 24, Port: 1}
	}
	if _, _, got, err := DecodeAnswerSources2(EncodeAnswerSources2(4, ed2k.Hash{}, src)); err != nil || len(got) != MaxSXSources {
		t.Fatalf("%d: %v", len(got), err)
	}
	if b := EncodeAnswerSources2(4, ed2k.Hash{}, append(src, src[0])); len(b) > MaxSXAnswerBytes {
		if _, _, _, err := DecodeAnswerSources2(b); err == nil {
			t.Fatal("more than MaxSXSources accepted")
		}
	}
	if MaxSXAnswerBytes != len(EncodeAnswerSources2(4, ed2k.Hash{}, src)) {
		t.Fatalf("MaxSXAnswerBytes %d, largest answer %d", MaxSXAnswerBytes, len(EncodeAnswerSources2(4, ed2k.Hash{}, src)))
	}
}

// A first-form answer is read in the version its size says: written in
// ours (4) or the answerer's.
func TestSXFirstFormVersionFromSize(t *testing.T) {
	file := ed2k.Hash{3}
	for _, tc := range []struct{ written, asked byte }{{4, 3}, {1, 3}, {2, 2}, {3, 3}, {4, 2}} {
		f, srcs, err := DecodeAnswerSources(EncodeAnswerSources(tc.written, file, sxSample()), tc.asked)
		if err != nil || f != file || len(srcs) != 2 || srcs[0].ID != sxSample()[0].ID || srcs[1].Port != 5000 {
			t.Fatalf("written v%d, asked v%d: %v %+v", tc.written, tc.asked, err, srcs)
		}
	}
	if _, srcs, err := DecodeAnswerSources(EncodeAnswerSources(4, file, nil), 3); err != nil || len(srcs) != 0 {
		t.Fatalf("empty answer: %v", err)
	}
}
