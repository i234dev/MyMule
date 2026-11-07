package download

import (
	"testing"
	"time"

	"mymule/internal/ed2k"
)

// serve has owner download up to n blocks (0 = all it can get) of the
// parts in avail, sending src's bytes.
func serve(t *testing.T, pf *PartFile, owner string, avail []bool, src []byte, n int) int {
	t.Helper()
	got := 0
	for n == 0 || got < n {
		blocks := pf.Reserve(owner, avail, 1)
		if len(blocks) == 0 {
			break
		}
		b := blocks[0]
		if _, err := pf.Write(owner, b.Start, src[b.Start:b.End]); err != nil {
			if err == ErrNotReserved {
				break
			}
			t.Fatal(err)
		}
		got++
	}
	return got
}

func blameSetup(t *testing.T) (pf *PartFile, data, garbage []byte) {
	t.Helper()
	data = testData(int(ed2k.PartSize)*2 + 5000) // three parts
	garbage = make([]byte, len(data))
	link, fh := linkFor(t, "film.mkv", data)
	pf, err := create(t.TempDir(), link, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pf.Close() })
	if err := pf.SetHashSet(fh.Parts); err != nil {
		t.Fatal(err)
	}
	return pf, data, garbage
}

// A peer that sends garbage alone is banned after banAfter bad parts,
// instead of making parts fail over and over so the file never completes.
func TestCorruptSenderBanned(t *testing.T) {
	pf, data, garbage := blameSetup(t)
	serve(t, pf, "evil:1", nil, garbage, 0)
	// banAfter different bad parts, plus at most one retry of each before
	// the last one (depending on which parts the random order picks).
	if c := pf.Corrupt(); !pf.Banned("evil:1") || c < banAfter || c > 2*banAfter-1 {
		t.Fatalf("banned=%v corrupt=%d after bad parts from one peer", pf.Banned("evil:1"), c)
	}
	if blocks := pf.Reserve("evil:1", nil, 5); len(blocks) != 0 {
		t.Fatal("banned peer got more blocks")
	}
	if done, _ := pf.Progress(); done != 0 {
		t.Fatalf("%d bytes from the banned peer kept", done)
	}
	serve(t, pf, "good:1", nil, data, 0)
	if !pf.Complete() || pf.Banned("good:1") {
		t.Fatalf("download not completed by the honest peer (complete=%v)", pf.Complete())
	}
}

// When several peers sent a part that fails, nobody is blamed; the part
// is then downloaded from one peer only, which finds the culprit.
func TestCorruptPartIsolated(t *testing.T) {
	pf, data, garbage := blameSetup(t)
	first := []bool{true, false, false}

	// Taking turns on the first part, one block each, until it fails.
	for pf.Corrupt() == 0 {
		a := serve(t, pf, "good:1", first, data, 1)
		b := serve(t, pf, "evil:1", first, garbage, 1)
		if a+b == 0 {
			t.Fatal("part 0 finished without failing")
		}
	}
	if pf.Banned("good:1") || pf.Banned("evil:1") {
		t.Fatal("someone banned for a part several peers sent")
	}

	// Now part 0 goes to one peer at a time.
	if serve(t, pf, "evil:1", first, garbage, 1) != 1 {
		t.Fatal("no block of the failed part for the first peer")
	}
	if len(pf.Reserve("good:1", first, 5)) != 0 {
		t.Fatal("a second peer got blocks of the isolated part")
	}
	serve(t, pf, "evil:1", first, garbage, 0) // fails alone: may retry once
	serve(t, pf, "evil:1", first, garbage, 0) // fails again: its copy is bad
	if len(pf.Reserve("evil:1", first, 5)) != 0 {
		t.Fatal("the peer whose copy of the part is bad still gets it")
	}
	if pf.Banned("good:1") {
		t.Fatal("the honest peer was banned")
	}
	serve(t, pf, "good:1", nil, data, 0)
	if !pf.Complete() {
		t.Fatal("download did not complete after the culprit was banned")
	}
}

// If the single peer of an isolated part goes away, another peer starts
// the part over after a while instead of waiting for it forever.
func TestIsolatedPartAbandoned(t *testing.T) {
	pf, data, _ := blameSetup(t)
	first := []bool{true, false, false}
	pf.mu.Lock()
	pf.isolate[0] = true
	pf.mu.Unlock()
	b := pf.Reserve("gone:1", first, 1)
	pf.Write("gone:1", b[0].Start, data[b[0].Start:b[0].End])
	pf.Release("gone:1", b[0]) // and disconnects
	if len(pf.Reserve("next:1", first, 1)) != 0 {
		t.Fatal("isolated part shared while its peer was active")
	}
	pf.mu.Lock()
	pf.lastWr[0] = pf.lastWr[0].Add(-isolateAbandon - 1)
	pf.mu.Unlock()
	serve(t, pf, "next:1", first, data, 0)
	if !pf.VerifiedParts()[0] {
		t.Fatal("abandoned isolated part not taken over")
	}
}

// A peer that reserves blocks and never sends them cannot stall the
// download: after reserveStale another peer takes them over, and the
// first one's late data is refused.
func TestStaleReservationTakenOver(t *testing.T) {
	pf, data, _ := blameSetup(t)
	held := pf.Reserve("idle:1", nil, 3)
	all := len(pf.Reserve("busy:1", nil, 1000)) // everything else
	ageReservations(pf, "idle:1")
	got := pf.Reserve("busy:1", nil, 1000)
	if len(got) != len(held) || all == 0 {
		t.Fatalf("took over %d of %d stale blocks", len(got), len(held))
	}
	b := held[0]
	if _, err := pf.Write("idle:1", b.Start, data[b.Start:b.End]); err != ErrNotReserved {
		t.Fatalf("late write by the old holder: %v", err)
	}
}

// A peer that holds the blocks of an isolated part and sends nothing for
// them loses them after reserveStale like anywhere else; the part is not
// kept from everybody for good.
func TestIsolatedPartStaleReservation(t *testing.T) {
	pf, data, _ := blameSetup(t)
	first := []bool{true, false, false}
	pf.mu.Lock()
	pf.isolate[0] = true
	pf.mu.Unlock()
	held := pf.Reserve("idle:1", first, 3)
	if len(held) == 0 {
		t.Fatal("setup: no blocks")
	}
	if len(pf.Reserve("next:1", first, 1)) != 0 {
		t.Fatal("isolated part shared while its peer was active")
	}
	ageReservations(pf, "idle:1")
	serve(t, pf, "next:1", first, data, 0)
	if !pf.VerifiedParts()[0] {
		t.Fatal("isolated part held for good by a peer that sends nothing")
	}
}

// An honest slow peer still sending its first pipelined block keeps the
// later ones, even when they were reserved long ago.
func TestSlowPeerKeepsPipelinedBlocks(t *testing.T) {
	pf, data, _ := blameSetup(t)
	first := []bool{true, false, false}
	a := pf.Reserve("A", first, 3)
	pf.mu.Lock()
	for _, b := range a {
		pf.resAt[b.Start] = time.Now().Add(-reserveStale - time.Minute)
	}
	pf.mu.Unlock()
	if _, err := pf.Write("A", a[0].Start, data[a[0].Start:a[0].Start+10240]); err != nil {
		t.Fatal(err)
	}
	for _, b := range pf.Reserve("B", first, 3) {
		if b == a[1] || b == a[2] {
			t.Fatal("an active slow peer's pipelined block was taken over")
		}
	}
	if _, err := pf.Write("A", a[2].Start, data[a[2].Start:a[2].Start+10240]); err != nil {
		t.Fatalf("the slow peer's data for its later block was refused: %v", err)
	}
}

// A peer that sends a bad copy of the same part again and again is not
// banned for it (a lone honest source with one damaged part), but bad
// data in several different parts is.
func TestBanNeedsDifferentBadParts(t *testing.T) {
	pf, _, garbage := blameSetup(t)
	first := []bool{true, false, false}
	for i := 0; i < 4; i++ {
		serve(t, pf, "flaky:1", first, garbage, 0)
	}
	if pf.Banned("flaky:1") || pf.Corrupt() != 2 {
		t.Fatalf("banned=%v corrupt=%d: one bad part must cost at most two tries and no ban", pf.Banned("flaky:1"), pf.Corrupt())
	}
	serve(t, pf, "flaky:1", []bool{false, true, false}, garbage, 0)
	if !pf.Banned("flaky:1") {
		t.Fatal("bad data in two different parts did not get the peer banned")
	}
}
