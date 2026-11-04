package share

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/ed2k"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestLibraryScanCacheAndRead(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("mule"), 300000) // 1.2 MB, single part
	os.WriteFile(filepath.Join(dir, "a.bin"), data, 0o644)
	os.WriteFile(filepath.Join(dir, "x.part"), []byte("never shared"), 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("never shared"), 0o644)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	cache := filepath.Join(t.TempDir(), "known.json")

	ctx, cancel := context.WithCancel(context.Background())
	l := NewLibrary([]string{dir}, cache, nil)
	go l.Run(ctx, time.Hour)
	waitFor(t, func() bool { return l.Status().Files == 1 && l.Status().ToHash == 0 })
	cancel()

	want, _ := ed2k.HashReader(bytes.NewReader(data), nil)
	f := l.Get(want.Root)
	if f == nil || f.FileName() != "a.bin" || f.FileSize() != int64(len(data)) || f.Partial() {
		t.Fatalf("shared file: %+v", f)
	}
	buf := make([]byte, 8)
	if err := ReadFull(f, buf, 4); err != nil || string(buf) != "mulemule" {
		t.Fatalf("read: %q %v", buf, err)
	}
	if err := ReadFull(f, buf, int64(len(data))-4); err == nil {
		t.Fatal("read past end succeeded")
	}

	// A second library with the same cache must not need to hash again.
	l2 := NewLibrary([]string{dir}, cache, nil)
	l2.scan(context.Background())
	if l2.Get(want.Root) == nil {
		t.Fatal("cached file not loaded")
	}

	// Removing the file unshares it on the next scan.
	os.Remove(filepath.Join(dir, "a.bin"))
	l2.scan(context.Background())
	if l2.Get(want.Root) != nil || l2.Status().Files != 0 {
		t.Fatal("deleted file still shared")
	}
}

type fakePartial struct{ h ed2k.Hash }

func (f fakePartial) FileHash() ed2k.Hash               { return f.h }
func (f fakePartial) FileName() string                  { return "p" }
func (f fakePartial) FileSize() int64                   { return 10 }
func (f fakePartial) PartHashes() []ed2k.Hash           { return nil }
func (f fakePartial) Avail() []bool                     { return []bool{true} }
func (f fakePartial) ReadAt([]byte, int64) (int, error) { return 0, ErrNotAvailable }
func (f fakePartial) Partial() bool                     { return true }

func TestPartialRegistration(t *testing.T) {
	l := NewLibrary(nil, "", nil)
	v := l.Version()
	h := ed2k.Hash{7}
	l.SetPartial(h, fakePartial{h})
	if l.Get(h) == nil || l.Status().Partial != 1 || l.Version() == v {
		t.Fatal("partial not registered")
	}
	l.SetPartial(h, nil)
	if l.Get(h) != nil {
		t.Fatal("partial not removed")
	}
}
