package share

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// link makes dir a link to target: a junction on Windows (no privileges
// needed), a symbolic link elsewhere.
func link(t *testing.T, target, dir string) {
	t.Helper()
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("cmd", "/c", "mklink", "/J", dir, target).Run()
	} else {
		err = os.Symlink(target, dir)
	}
	if err != nil {
		t.Skipf("cannot make a link here: %v", err)
	}
}

// A file of a shared folder is opened without leaving the folder: a folder
// swapped for a link to private files gives nothing, even for a file with
// the size and date that were listed.
func TestOpenInStaysInFolder(t *testing.T) {
	root, private := t.TempDir(), t.TempDir()
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(path, text string) {
		os.WriteFile(path, []byte(text), 0o644)
		os.Chtimes(path, at, at)
	}
	write(filepath.Join(private, "Made-up Film.mkv"), "private!")
	os.Mkdir(filepath.Join(root, "real"), 0o755)
	write(filepath.Join(root, "real", "Made-up Film.mkv"), "shared!!")
	size, mtime := int64(8), at.UnixNano()

	f, st, err := openIn(root, filepath.Join(root, "real", "Made-up Film.mkv"), size, mtime)
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("file of the folder: %v", err)
	}
	f.Close()

	link(t, private, filepath.Join(root, "swapped"))
	if f, _, err := openIn(root, filepath.Join(root, "swapped", "Made-up Film.mkv"), size, mtime); err == nil {
		f.Close()
		t.Fatal("opened a private file through a link out of the shared folder")
	}
	if id := treeFileID(root, filepath.Join(root, "swapped", "Made-up Film.mkv"), size, mtime); id != nil {
		t.Fatal("identity of a private file taken")
	}
	if _, _, _, err := hashFile(root, filepath.Join(root, "swapped", "Made-up Film.mkv"), size, mtime, nil); err == nil {
		t.Fatal("hashed a private file")
	}
}
