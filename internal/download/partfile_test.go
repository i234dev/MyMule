package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	if pf.Banned("peer") {
		t.Fatal("a peer was banned for a single bad part")
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

func TestCreateRejectsBadSizes(t *testing.T) {
	dir := t.TempDir()
	for _, size := range []int64{0, -1, ed2k.MaxFileSize + 1, 1 << 60} {
		if _, err := Create(dir, ed2k.Link{Name: "x", Size: size, Hash: ed2k.Hash{1}}); err == nil {
			t.Errorf("Create accepted size %d", size)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("rejected downloads left files behind: %v", left)
	}
}

// A state file with an impossible size, or one that is not valid at all,
// is skipped: the other downloads load and the daemon can start. An
// oversized one used to crash it on every start.
func TestLoadAllSkipsBadStateFiles(t *testing.T) {
	dir := t.TempDir()
	data := testData(5000)
	link, _ := linkFor(t, "good.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	pf.Close()
	huge := `{"hash":"31D6CFE0D16AE931B73C59D7E0C089C0","name":"huge.bin","size":1152921504606846976}`
	os.WriteFile(filepath.Join(dir, "31D6CFE0D16AE931B73C59D7E0C089C0.part.json"), []byte(huge), 0o600)
	os.WriteFile(filepath.Join(dir, "0123456789ABCDEF0123456789ABCDEF.part.json"), []byte("{not json"), 0o600)

	pfs, bad, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, p := range pfs {
			p.Close()
		}
	}()
	if len(pfs) != 1 || pfs[0].Hash != link.Hash {
		t.Fatalf("loaded %d downloads, want only the good one", len(pfs))
	}
	if len(bad) != 2 {
		t.Fatalf("reported %d bad state files, want 2: %v", len(bad), bad)
	}
}

func TestSanitizeNameHardening(t *testing.T) {
	cases := map[string]string{
		"Movie\u202evkm.exe":    "Movievkm.exe",
		"a\u200bb\u0085c.mkv":   "abc.mkv",
		"COM0.txt":              "_COM0.txt",
		"conin$":                "_conin$",
		"LPT¹.log":              "_LPT¹.log",
		"desktop.ini":           "_desktop.ini",
		"Free Movie.url":        "Free Movie.url_",
		"x.SCF":                 "x.SCF_",
		"x.lnk":                 "x.lnk_",
		"  spaced name.mkv  ":   "spaced name.mkv",
		"dir/../../etc/passwd":  "passwd",
		`..\..\Startup\run.bat`: "run.bat",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("Película larga ", 30) + ".mkv" // ~480 bytes
	got := SanitizeName(long)
	if len(got) > maxNameBytes || !strings.HasSuffix(got, ".mkv") || !utf8.ValidString(got) {
		t.Fatalf("long name: %d bytes, %q", len(got), got)
	}
}

// A download with a very long name used to fail its final move and stay
// at 100% forever; it now finishes under a shortened name.
func TestFinishLongName(t *testing.T) {
	dir := t.TempDir()
	data := testData(3000)
	link, _ := linkFor(t, strings.Repeat("Nombre muy largo ", 25)+".mkv", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) || len(filepath.Base(out)) > maxNameBytes {
		t.Fatalf("finished as %q (%d bytes)", filepath.Base(out), len(filepath.Base(out)))
	}
}

// A file of exactly one part (9,728,000 bytes) has a two-entry hash set
// (the part and an empty one) and its file hash is computed over both, so
// the part must be checked against the hash set, not the file hash. It
// used to fail verification forever and never complete.
func TestExactlyOnePart(t *testing.T) {
	dir := t.TempDir()
	data := testData(ed2k.PartSize)
	link, fh := linkFor(t, "exact.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	if !pf.NeedsHashSet() {
		t.Fatal("a file of exactly one part needs the hash set")
	}
	if err := pf.SetHashSet(fh.Parts); err != nil {
		t.Fatal(err)
	}
	for _, b := range pf.Reserve("peer", nil, 100) {
		pf.Write("peer", b.Start, data[b.Start:b.End])
	}
	if !pf.Complete() || pf.Corrupt() != 0 {
		t.Fatalf("complete=%v corrupt=%d", pf.Complete(), pf.Corrupt())
	}
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("final file differs")
	}
}

// When the final step fails, the download stays as it was (open,
// complete, its state kept) so it can be finished later.
func TestFinishFailureCanBeRetried(t *testing.T) {
	dir := t.TempDir()
	data := testData(5000)
	link, _ := linkFor(t, "retry.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	blocked := filepath.Join(dir, "not-a-folder")
	os.WriteFile(blocked, []byte("x"), 0o644)
	if _, err := pf.Finish(filepath.Join(blocked, "incoming")); err == nil {
		t.Fatal("Finish into a path under a file succeeded")
	}
	if !pf.Complete() {
		t.Fatal("download no longer complete after a failed Finish")
	}
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("final file differs")
	}
}

// A state file that cannot be saved when finishing (a full disk) leaves
// the download open and usable, not closed until a restart.
func TestFinishSaveFailureKeepsItOpen(t *testing.T) {
	dir := t.TempDir()
	data := testData(5000)
	link, _ := linkFor(t, "fulldisk.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	pf.mu.Lock()
	pf.dirty = true
	meta := pf.metaPath
	pf.mu.Unlock()
	os.Remove(meta)
	os.Mkdir(meta, 0o755) // the state file cannot be written
	if _, err := pf.Finish(filepath.Join(dir, "incoming")); err == nil {
		t.Fatal("Finish succeeded without saving the state")
	}
	pf.mu.Lock()
	open := pf.f != nil
	pf.mu.Unlock()
	if !open {
		t.Fatal("download left closed")
	}
	if _, err := pf.ReadVerified(make([]byte, 100), 0); err != nil {
		t.Fatalf("not readable (cannot be shared): %v", err)
	}
	os.Remove(meta)
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("final file differs")
	}
}

// Copies between disks go through a temporary name: the final name never
// holds a half-written file, and nothing is left behind on failure.
func TestCopyFileAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	os.WriteFile(src, testData(70000), 0o644)
	dst := filepath.Join(dir, "out", "dst.bin")
	if err := copyFile(src, dst); err == nil {
		t.Fatal("copy into a missing folder succeeded")
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "out", "*.mymule-tmp"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	if a, _ := os.ReadFile(src); !bytes.Equal(a, mustRead(t, dst)) {
		t.Fatal("copy differs")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// After the data file was closed for a move that then failed, reopen
// leaves the download usable for another attempt.
func TestReopenAfterFailedMove(t *testing.T) {
	dir := t.TempDir()
	data := testData(5000)
	link, _ := linkFor(t, "reopen.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	pf.Close() // as Finish does just before moving
	if _, err := pf.Finish(filepath.Join(dir, "incoming")); err == nil {
		t.Fatal("Finish of a closed file succeeded")
	}
	if err := pf.reopen(); err != nil {
		t.Fatal(err)
	}
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatalf("Finish after reopen: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("final file differs")
	}
}

// ReserveName must not loop when a name cannot even be checked (here, a
// name longer than file systems allow): the old uniquePath tried " (1)",
// " (2)"... forever, which hung the final step of the download.
func TestReserveNameDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := ReserveName(filepath.Join(dir, strings.Repeat("a", 400)+".mkv"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a 400-byte name was reserved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReserveName hung on a name that cannot be created")
	}
	p := filepath.Join(dir, "x.mkv")
	if got, err := ReserveName(p); err != nil || got != p {
		t.Fatalf("free name: %s %v", got, err)
	}
	if got, err := ReserveName(p); err != nil || got != filepath.Join(dir, "x (1).mkv") {
		t.Fatalf("taken name: %s %v", got, err)
	}
}

// Downloads finishing at the same time with the same name each get their
// own file (a check followed by a rename could give two the same name, and
// one would overwrite the other).
func TestReserveNameConcurrent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "same.mkv")
	names := make(chan string, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := ReserveName(p)
			if err != nil {
				t.Error(err)
			}
			names <- n
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for n := range names {
		if seen[n] {
			t.Fatalf("name %s given twice", n)
		}
		seen[n] = true
	}
}

// A download that does not fit in the free space is refused at once,
// instead of failing with a full disk halfway through.
func TestCreateChecksFreeSpace(t *testing.T) {
	old := diskFree
	defer func() { diskFree = old }()
	diskFree = func(string) (uint64, bool) { return 500 << 20, true } // 500 MB free
	dir := t.TempDir()
	if _, err := Create(dir, ed2k.Link{Name: "big.mkv", Size: 2 << 30, Hash: ed2k.Hash{1}}); err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("2 GB download with 500 MB free: %v", err)
	}
	pf, err := Create(dir, ed2k.Link{Name: "small.mkv", Size: 100 << 20, Hash: ed2k.Hash{2}})
	if err != nil {
		t.Fatalf("100 MB download with 500 MB free: %v", err)
	}
	pf.Close()
	diskFree = func(string) (uint64, bool) { return 0, false } // unknown: no check
	if pf, err := Create(dir, ed2k.Link{Name: "any.mkv", Size: 2 << 30, Hash: ed2k.Hash{3}}); err != nil {
		t.Fatalf("free space unknown: %v", err)
	} else {
		pf.Close()
	}
}

// The real free-space query answers on this system.
func TestFreeSpaceWorks(t *testing.T) {
	if n, ok := freeSpace(t.TempDir()); ok && n == 0 {
		t.Fatal("0 bytes free reported for the temporary folder")
	}
}

// The manager checks free space when adding a download, unless told not
// to (tests with big made-up downloads).
func TestManagerFreeSpaceCheck(t *testing.T) {
	old := diskFree
	defer func() { diskFree = old }()
	diskFree = func(string) (uint64, bool) { return 100 << 20, true }
	big := ed2k.Link{Name: "big.mkv", Size: 2 << 30, Hash: ed2k.Hash{7}}
	m := NewManager(Config{TempDir: t.TempDir()})
	if err := m.AddLink(big); err == nil {
		t.Fatal("2 GB download added with 100 MB free")
	}
	m = NewManager(Config{TempDir: t.TempDir(), SkipFreeSpaceCheck: true})
	if err := m.AddLink(big); err != nil {
		t.Fatalf("with the check off: %v", err)
	}
	_, files := m.Status()
	for _, f := range files {
		m.Cancel(f.Hash)
	}
}

// A crash after the file was moved but before its state file was removed
// does not download it all again: the next start sees it finished and
// removes the state file. If the data file is still there, the download
// resumes as usual.
func TestFinishCutShort(t *testing.T) {
	dir := t.TempDir()
	data := testData(5000)
	link, _ := linkFor(t, "cutshort.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	meta := pf.metaPath
	// Right before the move, the state already says where it goes.
	var saidBefore string
	testBeforeMove = func(metaPath, dst string) {
		b, _ := os.ReadFile(metaPath)
		var m partMeta
		json.Unmarshal(b, &m)
		if m.Finishing == dst {
			saidBefore = dst
		}
	}
	defer func() { testBeforeMove = nil }()
	out, err := pf.Finish(filepath.Join(dir, "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	if saidBefore != out {
		t.Fatal("the state did not say where the file was going before it moved")
	}
	// What a crash right after the move leaves: the state, saying where
	// the file was going.
	pf.mu.Lock()
	pf.deleted, pf.finishing = false, out
	pf.saveLocked()
	pf.mu.Unlock()
	if _, err := os.Stat(meta); err != nil {
		t.Fatal(err)
	}
	pfs, bad, err := LoadAll(dir)
	if err != nil || len(pfs) != 0 || len(bad) != 1 || !errors.Is(bad[0], ErrFinished) {
		t.Fatalf("loaded %d, bad %v, %v", len(pfs), bad, err)
	}
	if _, err := os.Stat(meta); !os.IsNotExist(err) {
		t.Fatal("state file left")
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("the finished file was touched")
	}
	if _, err := os.Stat(strings.TrimSuffix(meta, ".json")); !os.IsNotExist(err) {
		t.Fatal("an empty data file was made again")
	}
}

// A state file naming a finished file that does not match (another size)
// is not taken as finished; nor while the data file is still there.
func TestFinishingNotTrusted(t *testing.T) {
	dir := t.TempDir()
	data := testData(5000)
	link, _ := linkFor(t, "nottrusted.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	// A file of the right size at the path, but the data still here: not
	// moved, so not finished.
	same := filepath.Join(dir, "same-size.bin")
	os.WriteFile(same, make([]byte, len(data)), 0o644)
	pf.mu.Lock()
	pf.finishing, pf.dirty = same, true
	pf.saveLocked()
	pf.mu.Unlock()
	pf.Close()
	again, err := Open(pf.metaPath)
	if err != nil {
		t.Fatalf("data still there, file of its size named: %v", err)
	}
	again.Close()
	other := filepath.Join(dir, "other.bin")
	os.WriteFile(other, []byte("short"), 0o644)
	pf.mu.Lock()
	pf.finishing, pf.dirty = other, true
	pf.saveLocked()
	pf.mu.Unlock()
	again, err = Open(pf.metaPath)
	if err != nil {
		t.Fatalf("data still there: %v", err)
	}
	again.Close()
	os.Remove(pf.dataPath)
	again, err = Open(pf.metaPath)
	if errors.Is(err, ErrFinished) {
		t.Fatal("a file of another size taken as the finished one")
	}
	if again != nil {
		again.Close()
	}
	if got, _ := os.ReadFile(other); string(got) != "short" {
		t.Fatal("the named file was touched")
	}
}

// A failed move takes the "finishing" mark back.
func TestFinishFailureClearsFinishing(t *testing.T) {
	dir := t.TempDir()
	data := testData(5000)
	link, _ := linkFor(t, "clears.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	pf.Close()
	defer pf.Close()
	// As if the move had just failed: marked, then reopened.
	pf.mu.Lock()
	pf.finishing = filepath.Join(dir, "incoming", "x")
	pf.mu.Unlock()
	if err := pf.reopen(); err != nil {
		t.Fatal(err)
	}
	pf.Save()
	b, _ := os.ReadFile(pf.metaPath)
	if strings.Contains(string(b), "finishing") {
		t.Fatalf("mark kept: %s", b)
	}
}
