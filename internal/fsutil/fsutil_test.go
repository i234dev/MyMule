package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := WriteFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("content %q", b)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// Links planted next to the file, under the old predictable temporary
// name or as the file itself, must not make the write land elsewhere.
func TestWriteFileDoesNotFollowLinks(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	victim := filepath.Join(other, "victim")
	os.WriteFile(victim, []byte("untouched"), 0o600)
	p := filepath.Join(dir, "known.json")
	if err := os.Symlink(victim, p+".tmp"); err != nil {
		t.Skipf("cannot create links here: %v", err)
	}
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte("ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "untouched" {
		t.Fatalf("written through a link: victim now %q", b)
	}
	if st, err := os.Lstat(p); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the link was not replaced by the file")
	}
}
