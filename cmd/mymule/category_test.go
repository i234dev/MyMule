package main

import (
	"os"
	"path/filepath"
	"testing"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/library"
	"mymule/internal/prefs"
)

// A finished download of a category goes to the category's folder with
// its name; a category that is gone leaves it where it is.
func TestFinishedToCategory(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `Z:\no-such-appdata-for-tests`)
	t.Setenv("APPDATA", `Z:\no-such-appdata-for-tests`)
	data, downloads := t.TempDir(), t.TempDir()
	music := filepath.Join(downloads, "Music")
	e := &engine{Manager: download.NewManager(download.Config{TempDir: t.TempDir()}),
		prefs: prefs.Open(filepath.Join(data, "preferences.json"), data),
		items: library.OpenStore(filepath.Join(data, "library.json"))}
	e.prefs.AllowRoots(downloads) // download_dir
	p := e.prefs.Get()
	p.Categories = []prefs.Category{{Name: "Music", Dir: music}}
	if err := e.prefs.Set(p); err != nil {
		t.Fatal(err)
	}
	var h ed2k.Hash
	h[0] = 1
	src := filepath.Join(downloads, "Made-up Band - Song.mp3")
	os.WriteFile(src, []byte("la la la"), 0o644)
	e.items.Put(h.String(), library.Item{Kind: library.KindCategory, Category: "Music"})
	if got, want := e.organize(h, "Made-up Band - Song.mp3", src), filepath.Join(music, "Made-up Band - Song.mp3"); got != want {
		t.Fatalf("moved to %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(music, "Made-up Band - Song.mp3")); err != nil {
		t.Fatal(err)
	}

	src = filepath.Join(downloads, "Made-up Book.epub")
	os.WriteFile(src, []byte("once upon"), 0o644)
	e.items.Put(h.String(), library.Item{Kind: library.KindCategory, Category: "Books"})
	if got := e.organize(h, "Made-up Book.epub", src); got != "" {
		t.Fatalf("moved to a category that does not exist: %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("not left in place:", err)
	}

	// A hidden file is never moved into a category's folder.
	src = filepath.Join(downloads, ".bash_profile")
	os.WriteFile(src, []byte("echo hi"), 0o644)
	e.items.Put(h.String(), library.Item{Kind: library.KindCategory, Category: "Music"})
	if got := e.organize(h, ".bash_profile", src); got != "" {
		t.Fatalf("hidden file moved to %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("hidden file not left in place:", err)
	}
}
