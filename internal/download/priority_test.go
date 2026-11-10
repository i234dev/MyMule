package download

import (
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
)

// With preview on, the first and last parts are taken before the others;
// both settings survive a restart.
func TestPreviewOrder(t *testing.T) {
	data := testData(int(ed2k.PartSize)*4 + 5000) // five parts
	link, fh := linkFor(t, "Example.Film.mkv", data)
	dir := t.TempDir()
	pf, err := create(dir, link, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	pf.SetHashSet(fh.Parts)
	pf.SetPreview(true)
	pf.SetPriority(High)
	perPart := int((ed2k.PartSize + peer.BlockSize - 1) / peer.BlockSize)
	blocks := pf.Reserve("peer:1", nil, perPart+1) // all of part 0, and the one block of the last
	seen := map[int]bool{}
	for _, b := range blocks {
		seen[int(b.Start/ed2k.PartSize)] = true
	}
	if len(seen) != 2 || !seen[0] || !seen[4] {
		t.Fatalf("parts taken first: %v", seen)
	}
	if err := pf.Save(); err != nil {
		t.Fatal(err)
	}
	re, err := Open(pf.metaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if !re.Preview() || re.Priority() != High {
		t.Fatal("not kept")
	}
}

// Downloads with a higher priority come first; finished ones cannot be
// changed.
func TestPriorityOrder(t *testing.T) {
	m := NewManager(Config{TempDir: t.TempDir()})
	var hs []ed2k.Hash
	for i, name := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		link, _ := linkFor(t, name, testData(1000+i))
		pf, err := create(t.TempDir(), link, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pf.Close() })
		m.Add(pf)
		hs = append(hs, link.Hash)
	}
	if _, err := m.SetPriority(hs[2], High); err != nil {
		t.Fatal(err)
	}
	m.SetPriority(hs[0], Low)
	if _, err := m.SetPriority(hs[1], 5); err == nil {
		t.Fatal("priority 5 accepted")
	}
	for range 20 {
		m.mu.Lock()
		order := m.byPriorityLocked()
		m.mu.Unlock()
		if order[0].pf.Hash != hs[2] || order[2].pf.Hash != hs[0] {
			t.Fatal("not ordered by priority")
		}
	}
	_, files := m.Status()
	for _, f := range files {
		if f.Hash == hs[2] && f.Priority != High {
			t.Fatalf("status priority %d", f.Priority)
		}
	}
	m.mu.Lock()
	m.files[hs[1]].finished = true
	m.mu.Unlock()
	if _, err := m.SetPriority(hs[1], High); err != ErrFinishedYet {
		t.Fatalf("finished download: %v", err)
	}
}
