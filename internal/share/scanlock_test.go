package share

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
)

// A shared folder on a disk that stops answering (a sleeping NAS, a dead
// network mount) must not stop the rest of MyMule: the scan never waits
// on the disk while holding the library's lock, which uploads, the
// download manager and the web status all need.
func TestHungDiskDoesNotBlockLibrary(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "one.bin"), []byte("first file"), 0o644)
	l := NewLibrary([]string{dir}, filepath.Join(t.TempDir(), "known.json"), nil)
	l.scan(context.Background()) // hashed and cached

	hung := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	testDiskHook = func(string) {
		once.Do(func() { close(hung) })
		<-release
	}
	defer func() { testDiskHook = nil }()

	// Each scan below looks at the disk: an unchanged file is checked, and
	// a cached file that is not shared yet gets its id read.
	for _, step := range []func(){
		func() {},
		func() {
			l.mu.Lock()
			l.disk = map[string]*diskFile{}
			l.byHash = map[ed2k.Hash]File{}
			l.mu.Unlock()
		},
	} {
		step()
		hung = make(chan struct{})
		once = sync.Once{}
		release = make(chan struct{})
		done := make(chan struct{})
		go func() { l.scan(context.Background()); close(done) }()
		select {
		case <-hung:
		case <-time.After(5 * time.Second):
			t.Fatal("scan never looked at the disk")
		}
		answered := make(chan struct{})
		go func() {
			h := ed2k.Hash{9}
			l.SetPartial(h, fakePartial{h})
			l.Get(h)
			l.Status()
			l.SetPartial(h, nil)
			close(answered)
		}()
		select {
		case <-answered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("the library waited for the hung disk")
		}
		close(release)
		<-done
	}
	if l.Status().Files != 1 {
		t.Fatalf("files after the scans: %+v", l.Status())
	}
}

// A complete file wins over a partial download of the same file, and when
// its path changes to other contents the partial one is offered again.
func TestIndexUpdatedInPlace(t *testing.T) {
	l := NewLibrary(nil, "", nil)
	h, other := ed2k.Hash{1}, ed2k.Hash{2}
	l.SetPartial(h, fakePartial{h})
	l.mu.Lock()
	l.addDiskLocked("/x/a.bin", cacheEntry{Size: 5, Hash: h}, nil)
	l.mu.Unlock()
	if f := l.Get(h); f == nil || f.Partial() {
		t.Fatal("complete file does not win")
	}
	l.mu.Lock()
	l.addDiskLocked("/x/a.bin", cacheEntry{Size: 5, Hash: other}, nil)
	l.mu.Unlock()
	if f := l.Get(h); f == nil || !f.Partial() || l.Get(other) == nil {
		t.Fatal("old hash not dropped")
	}
	if n := l.Status().Files; n != 2 {
		t.Fatalf("%d files", n)
	}
}

// known.json is written now and then while hashing, and always at the
// end, not once per file.
func TestCacheSavedInBatches(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.bin", i)), []byte(fmt.Sprint("file ", i)), 0o644)
	}
	cache := filepath.Join(t.TempDir(), "known.json")
	l := NewLibrary([]string{dir}, cache, nil)
	writes := 0
	testDiskHook = func(path string) {
		if _, err := os.Stat(cache); err == nil {
			writes++
			os.Remove(cache)
		}
	}
	defer func() { testDiskHook = nil }()
	l.scan(context.Background())
	if writes != 0 {
		t.Fatalf("known.json written %d times during the scan", writes)
	}
	var got map[string]cacheEntry
	b, err := os.ReadFile(cache)
	if err != nil || json.Unmarshal(b, &got) != nil || len(got) != 20 {
		t.Fatalf("known.json after the scan: %d entries, %v", len(got), err)
	}

	// Stopped half-way, what was hashed is still saved.
	os.Remove(cache)
	l2 := NewLibrary([]string{dir}, cache, nil)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	testDiskHook = func(string) {
		if n++; n == 5 {
			cancel()
		}
	}
	l2.scan(ctx)
	got = nil
	b, err = os.ReadFile(cache)
	if err != nil || json.Unmarshal(b, &got) != nil || len(got) == 0 || len(got) == 20 {
		t.Fatalf("known.json after a stopped scan: %d entries, %v", len(got), err)
	}
}

// A file moved while the scan looks at the disk is not shared again under
// its old name.
func TestMovedDuringScan(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.bin"), filepath.Join(dir, "b.bin")
	os.WriteFile(a, []byte("moving file"), 0o644)
	l := NewLibrary([]string{dir}, filepath.Join(t.TempDir(), "known.json"), nil)
	l.scan(context.Background())
	h := l.List()[0].FileHash()
	l.mu.Lock()
	l.disk = map[string]*diskFile{}
	l.byHash = map[ed2k.Hash]File{}
	l.mu.Unlock()
	moved := false
	testDiskHook = func(string) {
		if !moved { // Moved itself looks at the disk too
			moved = true
			os.Rename(a, b)
			l.Moved(a, b)
		}
	}
	defer func() { testDiskHook = nil }()
	l.scan(context.Background())
	l.mu.Lock()
	_, stale := l.disk[a]
	l.mu.Unlock()
	if f, ok := l.Get(h).(*diskFile); stale || !ok || f.path != b {
		t.Fatalf("old name still shared: %v %+v", stale, l.Get(h))
	}
}
