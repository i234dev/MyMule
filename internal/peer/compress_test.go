package peer

import (
	"bytes"
	"compress/zlib"
	"errors"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

func packets(t *testing.T, hash ed2k.Hash, start int64, packed []byte, big bool) []DataPart {
	t.Helper()
	var out []DataPart
	for off := 0; off < len(packed); off += CompressedPacketSize {
		end := min(off+CompressedPacketSize, len(packed))
		op, payload := EncodeCompressedPart(hash, start, uint32(len(packed)), packed[off:end], big)
		d, err := DecodeDataPart(&proto.Packet{Proto: proto.ProtoEMule, Opcode: op, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// A block compressed and split in packets comes out whole, in both forms.
func TestCompressedBlock(t *testing.T) {
	data := bytes.Repeat([]byte("example text that compresses well. "), BlockSize/35+1)[:BlockSize]
	packed, ok := CompressBlock(data)
	if !ok || len(packed) > len(data)/4 {
		t.Fatalf("compressed to %d of %d", len(packed), len(data))
	}
	for _, big := range []bool{false, true} {
		b := NewCompressedBlock(5<<20, int64(len(data)))
		var got []byte
		for i, d := range packets(t, ed2k.Hash{1}, 5<<20, packed, big) {
			out, err := b.Add(d)
			if err != nil {
				t.Fatalf("packet %d: %v", i, err)
			}
			got = out
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("big=%v: block differs", big)
		}
	}
	if _, ok := CompressBlock(random(BlockSize)); ok {
		t.Fatal("random data compressed")
	}
	if Compressible("Example.Film.MKV") || !Compressible("example.iso") {
		t.Fatal("compressible")
	}
}

// Compressed data that does not fit the block is refused, without
// keeping more than the block's worth in memory.
func TestCompressedBlockHostile(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, 1000)
	packed, _ := CompressBlock(data)
	part := func(start int64, total uint32, chunk []byte) DataPart {
		return DataPart{Hash: ed2k.Hash{1}, Start: start, PackedSize: total, Compressed: true, Data: chunk}
	}
	for name, ds := range map[string][]DataPart{
		"huge size":      {part(0, 1<<30, packed)},
		"wrong start":    {part(1, uint32(len(packed)), packed)},
		"size changes":   {part(0, uint32(len(packed)), packed[:2]), part(0, uint32(len(packed))+1, packed[2:])},
		"more than said": {part(0, uint32(len(packed)), append(append([]byte{}, packed...), 0))},
		"not zlib":       {part(0, 10, []byte("0123456789"))},
	} {
		b := NewCompressedBlock(0, int64(len(data)))
		var err error
		for _, d := range ds {
			if _, err = b.Add(d); err != nil {
				break
			}
		}
		if !errors.Is(err, ErrBadCompressed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Inflating to more or less than the block is refused too.
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(bytes.Repeat([]byte{'a'}, 1001))
	w.Close()
	if _, err := NewCompressedBlock(0, 1000).Add(part(0, uint32(z.Len()), z.Bytes())); !errors.Is(err, ErrBadCompressed) {
		t.Errorf("longer than the block: %v", err)
	}
	if _, err := NewCompressedBlock(0, 2000).Add(part(0, uint32(z.Len()), z.Bytes())); !errors.Is(err, ErrBadCompressed) {
		t.Errorf("shorter than the block: %v", err)
	}
}

func random(n int) []byte {
	b := make([]byte, n)
	x := uint32(12345)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}
