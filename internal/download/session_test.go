package download

import (
	"errors"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// A peer that sends a compressed block claiming 4 GB of packed data would
// make us buffer everything it sent until that size was reached: a
// compressed size above what the block can take ends the session on the
// first such packet, with nothing buffered or written.
func TestCompressedBlockEndsSession(t *testing.T) {
	data := testData(ed2k.PartSize + 5000)
	link, _ := linkFor(t, "packed.bin", data)
	pf, err := Create(t.TempDir(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	blk := pf.Reserve("peer", nil, 1)[0]
	s := &session{pf: pf, owner: "peer", pending: []Range{blk}}

	for _, op := range []byte{peer.OpCompressedPart, peer.OpCompressedPartI64} {
		var w proto.Writer
		w.Bytes(link.Hash[:])
		if op == peer.OpCompressedPartI64 {
			w.U64(uint64(blk.Start))
		} else {
			w.U32(uint32(blk.Start))
		}
		w.U32(0xFFFFFFFF) // claimed packed size: 4 GB
		w.Bytes(make([]byte, 10240))
		err := s.onData(&proto.Packet{Proto: proto.ProtoEMule, Opcode: op, Payload: w.B})
		if !errors.Is(err, peer.ErrBadCompressed) {
			t.Fatalf("opcode 0x%02X: err = %v, want ErrBadCompressed", op, err)
		}
		s.packed = nil
	}
	if done, _ := pf.Progress(); done != 0 {
		t.Fatalf("compressed data was written: %d bytes", done)
	}
}
