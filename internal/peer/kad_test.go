package peer

import (
	"testing"

	"mymule/internal/ed2k"
)

func TestHelloWithKad(t *testing.T) {
	h := NewHello(ed2k.Hash{1}, "tester", 1234, 4662, nil, 0)
	if h.KadVersion() != 0 || h.KadPort() != 0 {
		t.Fatal("Kad announced without Kad")
	}
	k := h.WithKad(4672, 8)
	got, err := DecodeHello(k.Encode(false), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.KadVersion() != 8 || got.KadPort() != 4672 {
		t.Fatalf("version %d, port %d", got.KadVersion(), got.KadPort())
	}
	// The other option bits are kept (large files).
	if got.Tags.Int(CTEMuleMiscOpts2)&^0x0F != miscOpts2 {
		t.Fatalf("misc options 2 = %#x", got.Tags.Int(CTEMuleMiscOpts2))
	}
	// The original hello is unchanged.
	if h.KadVersion() != 0 || len(h.Tags) == len(k.Tags) {
		t.Fatal("WithKad changed its receiver")
	}
}
