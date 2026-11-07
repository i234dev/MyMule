package prefs

import (
	"os"
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
