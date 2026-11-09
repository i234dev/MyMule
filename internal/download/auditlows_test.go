package download

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// A partial download of exactly one part's size has a hash set, and
// gives it to peers; below one part there is none.
func TestSharedPartHashSetAtOnePart(t *testing.T) {
	data := testData(int(ed2k.PartSize))
	link, fh := linkFor(t, "onepart.bin", data)
	pf, err := Create(t.TempDir(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	if err := pf.SetHashSet(fh.Parts); err != nil {
		t.Fatal(err)
	}
	if got := (sharedPart{pf}).PartHashes(); len(got) != len(fh.Parts) || len(got) < 2 {
		t.Fatalf("%d hashes, want %d", len(got), len(fh.Parts))
	}
	small, _ := linkFor(t, "small.bin", testData(5000))
	sp, _ := Create(t.TempDir(), small)
	defer sp.Close()
	if (sharedPart{sp}).PartHashes() != nil {
		t.Fatal("a hash set below one part")
	}
}

// What a state file says is not taken blindly: ranges that make no sense
// are dropped, parts called verified without a hash set wait for it, and
// a data file longer than the file is cut.
func TestOpenDistrustsState(t *testing.T) {
	dir := t.TempDir()
	data := testData(int(ed2k.PartSize) + 5000) // two parts: needs a hash set
	link, _ := linkFor(t, "state.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data)
	meta, dataPath := pf.metaPath, pf.dataPath
	pf.Close()

	// Hand-edit the state: no hash set, every part verified, nonsense
	// ranges; and a data file with bytes past the end.
	s, _ := os.ReadFile(meta)
	st := string(s)
	if i := strings.Index(st, `"hashset"`); i >= 0 {
		j := strings.Index(st[i:], "]")
		st = st[:i] + `"hashset": null` + st[i+j+1:]
	}
	st = strings.Replace(st, `"done": [`, `"done": [[-5, 10], [100, 50], [99999999999, 99999999999999],`, 1)
	st = regexp.MustCompile(`"verified": \[[^\]]*\]`).ReplaceAllString(st, `"verified": [true, true]`)
	if !strings.Contains(st, `[true, true]`) {
		t.Fatalf("state not edited: %s", st)
	}
	os.WriteFile(meta, []byte(st), 0o600)
	f, _ := os.OpenFile(dataPath, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("junk past the end"))
	f.Close()

	again, err := Open(meta)
	if err != nil {
		t.Fatalf("%v\n%s", err, st)
	}
	defer again.Close()
	for p, v := range again.verified {
		if v {
			t.Fatalf("part %d verified without a hash set", p)
		}
	}
	for _, r := range again.done {
		if r.Start < 0 || r.End <= r.Start || r.End > again.Size {
			t.Fatalf("range %v kept", r)
		}
	}
	if fi, _ := os.Stat(dataPath); fi.Size() != int64(len(data)) {
		t.Fatalf("data file %d bytes, want %d", fi.Size(), len(data))
	}
}

// Several hash sets that do not fit say, once, that the link's size is
// probably wrong.
func TestBadHashSetsWarn(t *testing.T) {
	s, _, f := sxSession(t, peer.Hello{})
	f.pf.mu.Lock()
	f.pf.hashSet = nil // not known yet: peers are asked for it
	f.pf.mu.Unlock()
	var warned int
	s.m.cfg.Log = func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "size in its link is probably wrong") {
			warned++
		}
	}
	var w proto.Writer
	w.Bytes(f.pf.Hash[:])
	w.U16(2)
	w.Bytes(make([]byte, 32)) // two made-up part hashes
	for range badSetsWarn + 2 {
		s.handle(&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: peer.OpHashSetAnswer, Payload: w.B})
	}
	if warned != 1 {
		t.Fatalf("warned %d times", warned)
	}
}
