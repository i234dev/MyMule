package upload

import (
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
)

// The file status uses eMule's part count: size/PartSize + 1, one more
// than the real number of parts for exact multiples of the part size.
func TestFileStatusPartCount(t *testing.T) {
	for _, c := range []struct {
		size int64
		want int
	}{
		{ed2k.PartSize * 2, 3},
		{ed2k.PartSize*2 + 1, 3},
		{ed2k.PartSize - 1, 1},
	} {
		parts := int((c.size + ed2k.PartSize - 1) / ed2k.PartSize)
		avail := make([]bool, parts)
		avail[0] = true
		b := fileStatus(ed2k.Hash{1}, c.size, avail)
		_, got, err := peer.DecodeFileStatus(b)
		if err != nil || len(got) != c.want || !got[0] {
			t.Errorf("size %d: %d parts announced (err %v), want %d", c.size, len(got), err, c.want)
		}
		if c.want > parts && got[c.want-1] {
			t.Errorf("size %d: the empty extra part is announced as available", c.size)
		}
	}
}
