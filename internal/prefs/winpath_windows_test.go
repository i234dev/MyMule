package prefs

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A short 8.3 name (PROGRA~1) for a new folder inside a system folder is
// recognised as that system folder.
func TestLibraryFolderShortName(t *testing.T) {
	awayFromAppData(t)
	long := os.Getenv("ProgramFiles")
	in, _ := syscall.UTF16PtrFromString(long)
	buf := make([]uint16, 260)
	n, err := syscall.GetShortPathName(in, &buf[0], uint32(len(buf)))
	short := syscall.UTF16ToString(buf[:n])
	if err != nil || n == 0 || short == long {
		t.Skip("no short names on this disk")
	}
	if err := checkFolder(filepath.Join(short, "MyMuleTestNotThere", "Movies"), nil, nil, nil); err == nil {
		t.Errorf("%s accepted", filepath.Join(short, "MyMuleTestNotThere", "Movies"))
	}
}

// A junction (which filepath.EvalSymlinks no longer follows) to a system
// folder is refused like the folder itself; one to a normal folder is fine.
func TestLibraryFolderJunction(t *testing.T) {
	awayFromAppData(t)
	dir := t.TempDir()
	junction := func(name, target string) string {
		link := filepath.Join(dir, name)
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot make a junction: %v %s", err, out)
		}
		return link
	}
	bad := junction("lib", os.Getenv("SystemRoot"))
	if err := checkFolder(filepath.Join(bad, "Movies"), nil, nil, nil); err == nil {
		t.Errorf("junction to %s accepted", os.Getenv("SystemRoot"))
	}
	normal := filepath.Join(dir, "real")
	os.Mkdir(normal, 0o755)
	good := junction("good", normal)
	if err := checkFolder(filepath.Join(good, "Movies"), nil, nil, nil); err != nil {
		t.Errorf("junction to a normal folder refused: %v", err)
	}
}
