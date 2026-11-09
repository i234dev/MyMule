package aich

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"testing"
)

// pattern is test data: byte i is byte(i*31 + i>>11).
type pattern struct{ off, size int64 }

func (p *pattern) Read(b []byte) (int, error) {
	if p.off >= p.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(b)), p.size-p.off))
	for i := range n {
		o := p.off + int64(i)
		b[i] = byte(o*31 + o>>11)
	}
	p.off += int64(n)
	return n, nil
}

func (p *pattern) ReadAt(b []byte, off int64) error {
	q := pattern{off: off, size: p.size}
	_, err := io.ReadFull(&q, b)
	return err
}

// The roots aMule computed for the same data (from its known.met): the
// tree's shape (the odd block or part to the left in a left branch, to
// the right in a right one; parts, then blocks) must be eMule's exactly.
func TestRootsAsAMule(t *testing.T) {
	const B, P = BlockSize, PartSize
	for size, want := range map[int64]string{
		1:           "LOUTZHNQZ74T6UVVEHLUEDSD63W2E6CP",
		B - 1:       "NQGV7ICRAYIR2VEJOGJGPN33YSWKQELC",
		B:           "3CAR43A4UCISWHU4ALC7JUGJRUZP5FAY",
		B + 1:       "M6NKSBMGQJBY7SLSYUOKJ2FGSHAIFLLB",
		2*B + 5:     "25JN4YJMYVOWYPIL634NLV4BJ232HUOB",
		P - 1:       "XXS4GGH4CC4UHAMOICE4ONHNX6P57FZT",
		P:           "UW4IMYFKMCVNE2RARBV7PDZKZIIBA6WY",
		P + 1:       "ZBUEI67U46SGOWYJR5JZ4GPIRHYG6R2Q",
		P + B:       "DOIJA2BBAU4LT6UOXNBS3NDSZB4CL3CL",
		2 * P:       "KGF56JJ3PTEBXNNHLML2A64EHUYZXAMO",
		3*P + 12345: "KP6NVROBAZB2BRPSC5RMMNCBYJUZV3SY",
		5*P + 7:     "5QUGD2AZU37VFSVOJ56POL3MRK7XC2Q6",
		7*P - 3:     "3B54ACOLWGZJLU56YZKNRYSYBLBQX5CF",
	} {
		tr, err := FromReader(&pattern{size: size}, uint64(size))
		if err != nil {
			t.Fatal(err)
		}
		if got := tr.Root().String(); got != want {
			t.Errorf("size %d: root %s, aMule %s", size, got, want)
		}
		if len(tr.Parts) != int((size+P-1)/P) {
			t.Errorf("size %d: %d parts", size, len(tr.Parts))
		}
		// Written in odd pieces, the same.
		h := NewHasher(uint64(size))
		src := &pattern{size: size}
		buf := make([]byte, 77777)
		for {
			n, err := src.Read(buf[:1+rand.Intn(len(buf))])
			h.Write(buf[:n])
			if err != nil {
				break
			}
		}
		if tr2, err := h.Tree(); err != nil || tr2.Root() != tr.Root() {
			t.Errorf("size %d: in pieces %v", size, err)
		}
	}
}

func TestHashText(t *testing.T) {
	h, err := ParseHash("3cAR43A4UCISWHU4ALC7JUGJRUZP5FAY")
	if err != nil || h.String() != "3CAR43A4UCISWHU4ALC7JUGJRUZP5FAY" {
		t.Fatalf("%v %s", err, h)
	}
	for _, bad := range []string{"", "3CAR43A4UCISWHU4ALC7JUGJRUZP5FA", "3CAR43A4UCISWHU4ALC7JUGJRUZP5FAY7", "1CAR43A4UCISWHU4ALC7JUGJRUZP5FAY"} {
		if _, err := ParseHash(bad); err == nil {
			t.Errorf("%q read", bad)
		}
	}
	if _, err := (&Hasher{size: 3, sha: nil}).Tree(); err == nil {
		t.Fatal("tree of missing data")
	}
	if _, err := NewHasher(3).Write([]byte("four")); err == nil {
		t.Fatal("more data than the size taken")
	}
}

// Recovery data built for each part proves its block hashes against the
// root; anything changed in it does not.
func TestRecoveryRoundTrip(t *testing.T) {
	const P = PartSize
	for _, size := range []int64{2*BlockSize + 5, P, P + BlockSize + 1, 3*P + 12345, 5*P + 7} {
		src := &pattern{size: size}
		tr, err := FromReader(&pattern{size: size}, uint64(size))
		if err != nil {
			t.Fatal(err)
		}
		root := tr.Root()
		for p := range tr.Parts {
			if !CanRecover(uint64(size), p) {
				continue
			}
			blocks, err := BlockHashes(uint64(size), p, src.ReadAt)
			if err != nil {
				t.Fatal(err)
			}
			data, err := tr.Recovery(p, blocks)
			if err != nil {
				t.Fatalf("size %d part %d: %v", size, p, err)
			}
			got, err := Verify(uint64(size), p, root, data)
			if err != nil || len(got) != len(blocks) {
				t.Fatalf("size %d part %d: %v", size, p, err)
			}
			for i := range got {
				if got[i] != blocks[i] {
					t.Fatalf("size %d part %d block %d differs", size, p, i)
				}
			}
			// Any byte changed: refused (but the empty 32-bit list at
			// the end, which is not read, as in eMule).
			for i := 0; i < len(data)-2; i += 7 {
				bad := bytes.Clone(data)
				bad[i] ^= 0x40
				if _, err := Verify(uint64(size), p, root, bad); err == nil {
					t.Fatalf("size %d part %d: byte %d changed and taken", size, p, i)
				}
			}
			// Another part's data, or cut short: refused.
			if _, err := Verify(uint64(size), (p+1)%len(tr.Parts), root, data); err == nil && len(tr.Parts) > 1 {
				t.Fatalf("size %d: part %d data taken for another part", size, p)
			}
			if _, err := Verify(uint64(size), p, root, data[:len(data)-3]); err == nil {
				t.Fatal("short data taken")
			}
			// Data for wrong block hashes is not built.
			blocks[0][0] ^= 1
			if _, err := tr.Recovery(p, blocks); err == nil {
				t.Fatal("recovery data built from wrong block hashes")
			}
		}
	}
}

// Above 4 GB the identifiers are 32-bit, in the second list.
func TestRecoveryLargeFile(t *testing.T) {
	const size = 5<<30 + 12345
	parts := int((size + PartSize - 1) / PartSize)
	tr := &Tree{Size: size, Parts: make([]Hash, parts)}
	rnd := rand.New(rand.NewSource(1))
	blocksOf := map[int][]Hash{}
	for _, p := range []int{0, 300, parts - 1} {
		bl := make([]Hash, PartBlocks(size, p))
		for i := range bl {
			rnd.Read(bl[i][:])
		}
		blocksOf[p] = bl
		tr.Parts[p], _ = PartHash(size, p, bl)
	}
	for i := range tr.Parts {
		if blocksOf[i] == nil {
			rnd.Read(tr.Parts[i][:])
		}
	}
	root := tr.Root()
	for p, bl := range blocksOf {
		data, err := tr.Recovery(p, bl)
		if err != nil {
			t.Fatal(err)
		}
		if binary.LittleEndian.Uint16(data) != 0 {
			t.Fatal("16-bit list not empty")
		}
		if _, err := Verify(size, p, root, data); err != nil {
			t.Fatalf("part %d: %v", p, err)
		}
	}
	// A file under 4 GB uses the first list.
	small, _ := FromReader(&pattern{size: 3 * PartSize}, 3*PartSize)
	bl, _ := BlockHashes(3*PartSize, 1, (&pattern{size: 3 * PartSize}).ReadAt)
	data, _ := small.Recovery(1, bl)
	if binary.LittleEndian.Uint16(data) == 0 || binary.LittleEndian.Uint16(data[len(data)-2:]) != 0 {
		t.Fatal("small file: not in the 16-bit list")
	}
}

func TestCanRecover(t *testing.T) {
	for _, c := range []struct {
		size uint64
		p    int
		ok   bool
	}{{BlockSize, 0, false}, {BlockSize + 1, 0, true}, {PartSize + BlockSize, 1, false}, {PartSize + BlockSize + 1, 1, true}, {PartSize, 1, false}} {
		if CanRecover(c.size, c.p) != c.ok {
			t.Errorf("%d part %d", c.size, c.p)
		}
	}
}
