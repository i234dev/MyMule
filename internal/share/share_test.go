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

func TestTreesAndMoved(t *testing.T) {
	incoming, library := t.TempDir(), t.TempDir()
	data := bytes.Repeat([]byte("tree"), 1000)
	src := filepath.Join(incoming, "done.mkv")
	os.WriteFile(src, data, 0o644)
	want, _ := ed2k.HashReader(bytes.NewReader(data), nil)

	l := NewLibrary([]string{incoming}, filepath.Join(t.TempDir(), "known.json"), nil)
	l.SetTrees([]string{library})
	l.AddComplete(src, want.Root, nil)
	if l.Get(want.Root) == nil {
		t.Fatal("not shared after download")
	}

	// Moved into a subfolder of a shared tree: still shared, not hashed again.
	dst := filepath.Join(library, "Example Movie (2001)", "Example Movie (2001).mkv")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.Rename(src, dst)
	l.Moved(src, dst)
	f := l.Get(want.Root)
	if f == nil || f.FileName() != "Example Movie (2001).mkv" || l.Status().ToHash != 0 {
		t.Fatalf("after move: %+v", f)
	}
	l.scan(context.Background())
	if l.Get(want.Root) == nil || l.Status().Files != 1 {
		t.Fatalf("lost after rescan: %d files", l.Status().Files)
	}

	// Files deep in a tree are found by a scan; hidden folders are skipped.
	other := bytes.Repeat([]byte("deep"), 1000)
	os.MkdirAll(filepath.Join(library, "Show", "Season 01"), 0o755)
	os.WriteFile(filepath.Join(library, "Show", "Season 01", "e1.mkv"), other, 0o644)
	os.MkdirAll(filepath.Join(library, ".trash"), 0o755)
	os.WriteFile(filepath.Join(library, ".trash", "x.mkv"), []byte("hidden"), 0o644)
	l.scan(context.Background())
	if l.Status().Files != 2 {
		t.Fatalf("tree scan: %d files", l.Status().Files)
	}
}

// Movie and series folders share only video, audio and subtitle files, and
// never what is in recycle bins or NAS index folders. Folders from the
// shared folder list keep sharing every kind of file, as before.
func TestTreesShareOnlyMedia(t *testing.T) {
	tree, plain := t.TempDir(), t.TempDir()
	put := func(dir, rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	put(tree, "Film (2001)/Film (2001).mkv", "video one")
	put(tree, "Film (2001)/Film (2001).es.SRT", "subtitles")
	put(tree, "Film (2001)/run.bat", "script")
	put(tree, "notes.txt", "private notes")
	put(tree, "photo.jpg", "a picture")
	put(tree, "#recycle/old.mkv", "deleted video")
	put(tree, "$RECYCLE.BIN/S-1-5/old2.mkv", "deleted video 2")
	put(tree, "@eaDir/thumb.mkv", "index")
	put(plain, "readme.txt", "shared on purpose")

	l := NewLibrary([]string{plain}, filepath.Join(t.TempDir(), "known.json"), nil)
	l.SetTrees([]string{tree})
	l.scan(context.Background())
	if n := l.Status().Files; n != 3 {
		t.Fatalf("%d files shared, want 3 (the video, its subtitles and the plain folder's file)", n)
	}

	// A finished download that is not media is not published from a tree.
	bat := filepath.Join(tree, "Film (2001)", "run.bat")
	l.AddComplete(bat, ed2k.Hash{7}, nil)
	if l.Get(ed2k.Hash{7}) != nil {
		t.Fatal("a .bat inside a movie folder was shared")
	}
}
