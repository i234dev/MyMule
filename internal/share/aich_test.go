package share

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"mymule/internal/aich"
	"mymule/internal/ed2k"
)

// A scan computes the AICH tree with the eD2K hashes, and the cache keeps
// it: a new library has it without hashing again. A file whose tree is
// not known gets it computed when asked; a partial one never.
func TestLibraryAICH(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("example data "), 900000) // over one part
	p := filepath.Join(dir, "a.bin")
	os.WriteFile(p, data, 0o644)
	want, _ := aich.FromReader(bytes.NewReader(data), uint64(len(data)))
	fh, _ := ed2k.HashReader(bytes.NewReader(data), nil)
	cache := filepath.Join(t.TempDir(), "known.json")

	l := NewLibrary([]string{dir}, cache, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.scan(ctx)
	if tr := l.AICH(fh.Root); tr == nil || tr.Root() != want.Root() {
		t.Fatal("scan did not compute the AICH tree")
	}
	l2 := NewLibrary([]string{dir}, cache, nil)
	l2.scan(ctx)
	if tr := l2.AICH(fh.Root); tr == nil || tr.Root() != want.Root() {
		t.Fatal("AICH tree not kept in the cache")
	}

	// Known without its tree (as from earlier versions): computed on demand.
	os.Remove(cache)
	l3 := NewLibrary([]string{dir}, cache, nil)
	l3.AddComplete(p, fh.Root, fh.Parts)
	go l3.aichWorker(ctx)
	if l3.AICH(fh.Root) != nil {
		t.Fatal("tree before it was computed")
	}
	waitFor(t, func() bool { return l3.AICH(fh.Root) != nil })
	if l3.AICH(fh.Root).Root() != want.Root() {
		t.Fatal("computed tree differs")
	}
	// Saved too (before it is given out).
	l4 := NewLibrary([]string{dir}, cache, nil)
	l4.scan(ctx)
	if l4.AICH(fh.Root) == nil {
		t.Fatal("computed tree not saved")
	}

	l3.SetPartial(ed2k.Hash{7}, nil)
	if l3.AICH(ed2k.Hash{7}) != nil {
		t.Fatal("tree for a partial file")
	}
}
