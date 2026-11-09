package peer

import (
	"bytes"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

func TestReaskRoundTrip(t *testing.T) {
	file := ed2k.Hash{4, 2}
	status := []bool{true, false, true, true, false, false, false, false, true}
	for v := byte(1); v <= UDPVersion; v++ {
		b := EncodeReaskFilePing(v, file, status, 7)
		if f, err := DecodeReaskFilePing(b, v); err != nil || f != file {
			t.Fatalf("ping v%d: %v", v, err)
		}
		st, rank, err := DecodeReaskAck(EncodeReaskAck(v, status, 321), v)
		if err != nil || rank != 321 || (v >= 4) != (len(st) == len(status)) {
			t.Fatalf("ack v%d: %v %d %v", v, err, rank, st)
		}
		for i := range st {
			if st[i] != status[i] {
				t.Fatalf("ack v%d: part %d", v, i)
			}
		}
	}
	// eMule's bytes for version 4: hash, count 9, two bytes of bits,
	// complete sources.
	b := EncodeReaskFilePing(4, file, status, 7)
	want := append(append([]byte(nil), file[:]...), 9, 0, 0x0D, 0x01, 7, 0)
	if !bytes.Equal(b, want) {
		t.Fatalf("% X\nwant % X", b, want)
	}
	if st, rank, _ := DecodeReaskAck(EncodeReaskAck(4, nil, 5), 4); st != nil || rank != 5 {
		t.Fatal("complete file status")
	}
}

func TestReaskRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		b []byte
		v byte
	}{
		"short ping":     {make([]byte, 10), 4},
		"status too big": {append(make([]byte, 16), 0xFF, 0xFF), 4},
		"status cut":     {append(make([]byte, 16), 20, 0, 1), 4},
	} {
		if _, err := DecodeReaskFilePing(tc.b, tc.v); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := DecodeReaskAck([]byte{1}, 3); err == nil {
		t.Error("ack without a rank accepted")
	}
	if _, _, err := DecodeReaskAck([]byte{9, 0, 1}, 4); err == nil {
		t.Error("ack with a cut status accepted")
	}
}

func TestHelloClientUDP(t *testing.T) {
	h := NewHello(ed2k.Hash{1}, "me", 0x02000000, 4662, nil, 0).WithKad(4672, 8).WithClientUDP(4672)
	if h.UDPVersion() != UDPVersion || h.UDPPort() != 4672 || h.KadPort() != 4672 || h.SourceExchange1() != SXVersion {
		t.Fatalf("version %d, port %d, Kad port %d", h.UDPVersion(), h.UDPPort(), h.KadPort())
	}
	plain := NewHello(ed2k.Hash{1}, "me", 0, 4662, nil, 0)
	if plain.UDPVersion() != 0 || plain.UDPPort() != 0 {
		t.Fatal("client UDP offered without a port")
	}
	_ = proto.TagList{}
}
