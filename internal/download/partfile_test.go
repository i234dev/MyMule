package download

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mymule/internal/ed2k"
)

func TestRangeSet(t *testing.T) {
	var s RangeSet
	s.Add(Range{10, 20})
	s.Add(Range{30, 40})
	s.Add(Range{20, 30}) // bridges both
	if len(s) != 1 || s[0] != (Range{10, 40}) {
		t.Fatalf("merge: %v", s)
	}
	s.Remove(Range{15, 25})
	if len(s) != 2 || s[0] != (Range{10, 15}) || s[1] != (Range{25, 40}) {
		t.Fatalf("remove: %v", s)
	}
	if s.Covered(Range{0, 100}) != 20 || !s.Contains(Range{26, 39}) || s.Contains(Range{14, 16}) {
		t.Fatalf("covered/contains: %v", s)
	}
}

func testData(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*31 + i>>9)
	}
	return b
}

func linkFor(t *testing.T, name string, data []byte) (ed2k.Link, *ed2k.FileHashes) {
	t.Helper()
	fh, err := ed2k.HashReader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ed2k.Link{Name: name, Size: int64(len(data)), Hash: fh.Root}, fh
}

// download simulates peers serving blocks from data.
func download(t *testing.T, pf *PartFile, data []byte) {
	t.Helper()
	for {
		blocks := pf.Reserve("peer", nil, 3)
		if len(blocks) == 0 {
			return
		}
		for _, b := range blocks {
			if _, err := pf.Write("peer", b.Start, data[b.Start:b.End]); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPartFileMultiPartWithResume(t *testing.T) {
	dir := t.TempDir()
	data := testData(ed2k.PartSize*2 + 5000)
	link, fh := linkFor(t, "multi.bin", data)

	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	if !pf.NeedsHashSet() {
		t.Fatal("multi-part file should need a hash set")
	}
	// Download one part before we have the hashset, then "crash".
	for _, b := range pf.Reserve("p", []bool{true, false, false}, 100) {
		pf.Write("p", b.Start, data[b.Start:b.End])
	}
	pf.Close()

	pf, err = Open(filepath.Join(dir, link.Hash.String()+".part.json"))
	if err != nil {
		t.Fatal(err)
	}
	if done, _ := pf.Progress(); done != ed2k.PartSize {
		t.Fatalf("resumed progress = %d", done)
	}
	if err := pf.SetHashSet(fh.Parts); err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	if !pf.Complete() {
		t.Fatal("not complete")
	}
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, data) {
		t.Fatal("final file differs")
	}
}

// One-block-at-a-time reservations must stay in the same part.
func TestReserveConcentratesOnOnePart(t *testing.T) {
	data := testData(ed2k.PartSize * 3)
	link, _ := linkFor(t, "x", data)
	pf, err := Create(t.TempDir(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	first := pf.Reserve("a", nil, 1)[0]
	for i := 0; i < 5; i++ {
		b := pf.Reserve("a", nil, 1)[0]
		if b.Start/ed2k.PartSize != first.Start/ed2k.PartSize {
			t.Fatalf("block %d in part %d, first block in part %d", i, b.Start/ed2k.PartSize, first.Start/ed2k.PartSize)
		}
	}
}

func TestPartFileCorruption(t *testing.T) {
	dir := t.TempDir()
	data := testData(300000) // single part, verified against root hash
	link, _ := linkFor(t, "single.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), data...)
	bad[1000] ^= 0xFF
	// One pass only: a corrupt part is discarded and requested again, so
	// looping with bad data would never end.
	for _, b := range pf.Reserve("peer", nil, 100) {
		pf.Write("peer", b.Start, bad[b.Start:b.End])
	}
	if pf.Complete() || pf.Corrupt() != 1 {
		t.Fatalf("corrupt data accepted (corrupt=%d)", pf.Corrupt())
	}
	if done, _ := pf.Progress(); done != 0 {
		t.Fatalf("corrupt part not discarded: %d bytes", done)
	}
	download(t, pf, data)
	if !pf.Complete() {
		t.Fatal("not complete after re-download")
	}
	pf.Close()
}

func TestBadHashSetRejected(t *testing.T) {
	data := testData(ed2k.PartSize + 10)
	link, fh := linkFor(t, "x", data)
	pf, err := Create(t.TempDir(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	bad := append([]ed2k.Hash(nil), fh.Parts...)
	bad[0][0] ^= 1
	if pf.SetHashSet(bad) == nil {
		t.Fatal("bad hashset accepted")
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"normal.iso":       "normal.iso",
		"../../etc/passwd": "passwd",
		`a:b*c?.txt`:       "a_b_c_.txt",
		"CON.txt":          "_CON.txt",
		"trailing. ":       "trailing",
		"..":               "download",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A peer whose garbage made a part fail must not be able to send that
// block again once the part has been downloaded from others and verified:
// that would overwrite verified data with bytes of its choice.
func TestLateBlockCannotOverwriteVerifiedPart(t *testing.T) {
	dir := t.TempDir()
	data := testData(ed2k.PartSize*2 + 5000)
	link, fh := linkFor(t, "late.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	if err := pf.SetHashSet(fh.Parts); err != nil {
		t.Fatal(err)
	}
	onlyFirst := []bool{true, false, false}

	// The attacker gets one block of part 0 and sends garbage for it.
	blk := pf.Reserve("attacker", onlyFirst, 1)[0]
	evil := bytes.Repeat([]byte{0xEE}, int(blk.Len()))
	if _, err := pf.Write("attacker", blk.Start, evil); err != nil {
		t.Fatal(err)
	}
	// An honest peer fills the rest; the part fails, is discarded, and is
	// then downloaded again in full from the honest peer and verified.
	for {
		bs := pf.Reserve("honest", onlyFirst, 3)
		if len(bs) == 0 {
			break
		}
		for _, b := range bs {
			if _, err := pf.Write("honest", b.Start, data[b.Start:b.End]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if pf.Corrupt() != 1 || !pf.VerifiedParts()[0] {
		t.Fatalf("setup: corrupt=%d verified=%v", pf.Corrupt(), pf.VerifiedParts())
	}

	// The attacker still thinks it holds the block and sends it again.
	if _, err := pf.Write("attacker", blk.Start, evil); !errors.Is(err, ErrNotReserved) {
		t.Fatalf("late write to a verified part: err = %v, want ErrNotReserved", err)
	}
	// Nor may anyone write a block reserved for someone else, or data
	// that runs past the end of its block.
	// Part 1 (the middle one), so the block is never the file's last.
	other := pf.Reserve("honest", []bool{false, true, false}, 1)[0]
	if _, err := pf.Write("attacker", other.Start, data[other.Start:other.End]); !errors.Is(err, ErrNotReserved) {
		t.Fatalf("write to another peer's block: err = %v, want ErrNotReserved", err)
	}
	if _, err := pf.Write("honest", other.Start, data[other.Start:other.End+1]); !errors.Is(err, ErrNotReserved) {
		t.Fatalf("write past the block end: err = %v, want ErrNotReserved", err)
	}
	pf.Release("honest", other)

	download(t, pf, data)
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, data) {
		t.Fatal("final file differs from the original")
	}
}

// Finish hashes the whole file again: a verified part changed on disk
// afterwards is caught, marked for download again, and fixed.
func TestFinishRechecksEveryPart(t *testing.T) {
	dir := t.TempDir()
	data := testData(ed2k.PartSize + 5000)
	link, fh := linkFor(t, "recheck.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	if err := pf.SetHashSet(fh.Parts); err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	if !pf.Complete() {
		t.Fatal("not complete")
	}
	// Tamper with part 0 behind the part file's back.
	if _, err := pf.f.WriteAt([]byte("tampered"), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := pf.Finish(filepath.Join(dir, "incoming")); err == nil {
		t.Fatal("Finish accepted a file whose part changed after verification")
	}
	if pf.Complete() || pf.VerifiedParts()[0] || !pf.VerifiedParts()[1] {
		t.Fatalf("after failed check: verified=%v", pf.VerifiedParts())
	}
	download(t, pf, data)
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("final file differs from the original")
	}
}
