package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The pages in docs/ must be what the template and the texts produce:
// after changing docs/site, run 'go run ./tools/sitegen'.
func TestPagesUpToDate(t *testing.T) {
	root := filepath.Join("..", "..")
	pages, err := render(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != len(langs) {
		t.Fatalf("%d pages for %d languages", len(pages), len(langs))
	}
	for path, want := range pages {
		got, err := os.ReadFile(filepath.Join(root, path))
		got = bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")) // a Windows checkout
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is out of date: run 'go run ./tools/sitegen'", path)
		}
	}
}
