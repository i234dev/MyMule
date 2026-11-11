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

// Packets of a compressed block are no progress until the block is whole:
// a peer hopping between two blocks, finishing neither, neither keeps the
// session alive nor goes on unpunished.
func TestCompressedHoppingEndsSession(t *testing.T) {
	data := testData(ed2k.PartSize + 5000)
	link, _ := linkFor(t, "packed.bin", data)
	pf, err := Create(t.TempDir(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	blocks := pf.Reserve("peer", nil, 2)
	if len(blocks) < 2 {
		t.Fatalf("%d blocks reserved", len(blocks))
	}
	s := &session{pf: pf, owner: "peer", pending: blocks}
	packet := func(b Range) *proto.Packet {
		var w proto.Writer
		w.Bytes(link.Hash[:])
		w.U32(uint32(b.Start))
		w.U32(4000) // packed size: more than this packet
		w.Bytes(make([]byte, 1000))
		return &proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpCompressedPart, Payload: w.B}
	}
	if err := s.onData(packet(blocks[0])); err != nil {
		t.Fatal(err)
	}
	if !s.deadline.IsZero() {
		t.Fatal("part of a compressed block moved the deadline")
	}
	if err := s.onData(packet(blocks[1])); !errors.Is(err, peer.ErrBadCompressed) {
		t.Fatalf("hopping to another block: err = %v", err)
	}
}
