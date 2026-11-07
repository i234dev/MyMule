package download

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
)

// RangeSet against a plain byte map, with random operations.
func TestRangeSetMatchesModel(t *testing.T) {
	const size = 300
	rng := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 300; round++ {
		var s RangeSet
		var model [size]bool
		for op := 0; op < 60; op++ {
			a, b := int64(rng.IntN(size)), int64(rng.IntN(size))
			if a > b {
				a, b = b, a
			}
			add := rng.IntN(3) != 0
			if add {
				s.Add(Range{a, b})
			} else {
				s.Remove(Range{a, b})
			}
			for i := a; i < b; i++ {
				model[i] = add
			}
			var want RangeSet
			for i := int64(0); i < size; i++ {
				if !model[i] {
					continue
				}
				if n := len(want); n > 0 && want[n-1].End == i {
					want[n-1].End++
				} else {
					want = append(want, Range{i, i + 1})
				}
			}
			if len(want) == 0 && len(s) == 0 {
				continue
			}
			if !reflect.DeepEqual(s, want) {
				t.Fatalf("after %v %v: got %v, want %v", add, Range{a, b}, s, want)
			}
			q := Range{int64(rng.IntN(size)), int64(rng.IntN(size))}
			if q.Start > q.End {
				q.Start, q.End = q.End, q.Start
			}
			var covered int64
			for i := q.Start; i < q.End; i++ {
				if model[i] {
					covered++
				}
			}
			if got := s.Covered(q); got != covered {
				t.Fatalf("Covered(%v) = %d, want %d in %v", q, got, covered, s)
			}
		}
	}
}

// A peer sending its blocks as countless one-byte pieces in scattered
// order cannot make the download slow: only a few pieces per block are
// accepted, and bookkeeping stays fast.
func TestScatteredTinyWritesBounded(t *testing.T) {
	data := testData(int(ed2k.PartSize) * 3)
	link, fh := linkFor(t, "film.mkv", data)
	pf, err := create(t.TempDir(), link, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	pf.SetHashSet(fh.Parts)
	start := time.Now()
	blocks := pf.Reserve("peer", nil, 50)
	accepted := 0
	for _, b := range blocks {
		for off := b.Start + 1; off < b.End; off += 2 { // every other byte
			if _, err := pf.Write("peer", off, data[off:off+1]); err == nil {
				accepted++
			}
		}
	}
	if accepted != maxPiecesPerBlock*len(blocks) || len(pf.done) != accepted {
		t.Fatalf("%d pieces accepted, %d on record, for %d blocks", accepted, len(pf.done), len(blocks))
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took %v for %d blocks of %d bytes", el, len(blocks), peer.BlockSize)
	}
}
