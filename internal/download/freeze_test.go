package download

import (
	"context"
	"net"
	"testing"
	"time"

	"mymule/internal/ed2k"
)

// While one download is busy (its lock held, e.g. hashing a part), the
// manager's own lock stays free: everything else keeps working.
func TestStatusDoesNotHoldManagerWhileDownloadBusy(t *testing.T) {
	data := testData(4000)
	link, _ := linkFor(t, "film.mkv", data)
	pf, err := create(t.TempDir(), link, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	m := NewManager(Config{})
	m.files = map[ed2k.Hash]*File{link.Hash: {pf: pf, sources: map[string]*Source{}}}

	pf.mu.Lock()
	done := make(chan struct{})
	go func() { m.Status(); close(done) }()
	free := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if m.mu.TryLock() {
			m.mu.Unlock()
			free = true
			break
		}
	}
	pf.mu.Unlock()
	<-done
	if !free {
		t.Fatal("the manager stayed locked while waiting for a busy download")
	}
}

// The final check of a large file hashes it without holding the
// download's lock, so its progress can be read meanwhile.
func TestRecheckDoesNotHoldDownloadLock(t *testing.T) {
	data := testData(int(ed2k.PartSize)*3 + 100)
	link, fh := linkFor(t, "film.mkv", data)
	pf, err := create(t.TempDir(), link, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	pf.SetHashSet(fh.Parts)
	download(t, pf, data)
	done := make(chan error)
	go func() { done <- pf.recheck() }()
	reads := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if reads < 2 {
				t.Skipf("recheck finished too fast to tell (%d reads)", reads)
			}
			return
		default:
		}
		start := time.Now()
		pf.Progress()
		if el := time.Since(start); el > 20*time.Millisecond {
			t.Fatalf("Progress waited %v during the final check", el)
		}
		reads++
	}
}

// A cancelled download stays cancelled: a save that runs after the files
// were removed (the periodic save, a pause) does not write them back.
func TestSaveAfterDeleteWritesNothing(t *testing.T) {
	dir := t.TempDir()
	data := testData(4000)
	link, _ := linkFor(t, "film.mkv", data)
	pf, err := create(dir, link, false)
	if err != nil {
		t.Fatal(err)
	}
	b := pf.Reserve("peer", nil, 1)[0]
	pf.Write("peer", b.Start, data[b.Start:b.Start+100]) // dirty
	if err := pf.Delete(); err != nil {
		t.Fatal(err)
	}
	pf.Save()
	pf.Close()
	pfs, bad, _ := LoadAll(dir)
	if len(pfs) != 0 || len(bad) != 0 {
		t.Fatalf("cancelled download came back: %d loaded, %d bad", len(pfs), len(bad))
	}
}

// After a panic in a session we dialed, the source is released: it does
// not stay busy (never asked again) nor keep a connection slot.
func TestSessionPanicReleasesSource(t *testing.T) {
	testSessionHook = func() { panic("bad session") }
	defer func() { testSessionHook = nil }()
	m := NewManager(Config{Log: func(string, ...any) {}})
	f := &File{sources: map[string]*Source{}}
	src := &Source{key: "198.51.100.9:4662", file: f, busy: true}
	f.sources[src.key] = src
	m.active = 1
	a, b := net.Pipe()
	defer b.Close()
	m.runSession(context.Background(), a, true, src)
	m.mu.Lock()
	defer m.mu.Unlock()
	if src.busy || m.active != 0 {
		t.Fatalf("after a panic: source busy=%v, active connections=%d", src.busy, m.active)
	}
}
