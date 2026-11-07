package prefs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Windows accepts many spellings of the same folder. The ones the folder
// rules would not recognise are refused outright.
func TestWindowsPathForms(t *testing.T) {
	host, _ := os.Hostname()
	bad := []string{
		`\\?\C:\Users\someone`,
		`\\?\C:\Windows`,
		`\\.\C:\Windows`,
		`//?/C:/Windows`,
		`\??\C:\Windows`,
		`\\localhost\C$\Windows`,
		`\\127.0.0.1\c$\Users\someone`,
		`\\[::1]\share\x`,
		`\\0--1.ipv6-literal.net\share\x`,
		`\\` + host + `\Users\someone`,
		`\\nas\admin$\x`,
		`\\nas`,
		`C:\Windows.`,
		`C:\Windows \System32`,
		`C:\Movies\film.mkv:hidden`,
	}
	for _, p := range bad {
		if windowsPathProblem(p) == nil {
			t.Errorf("%s accepted", p)
		}
	}
	for _, p := range []string{`D:\Movies`, `C:\Users\someone\Videos\Series`, `\\nas\media\Movies`, `\\192.0.2.10\media`, `D:\.\Movies\..\Films`} {
		if err := windowsPathProblem(p); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
}

// The full check on Windows: device and loopback paths to the user's
// folder or the system folder are refused, as their normal spelling is.
func TestLibraryFolderWindowsSpellings(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows paths")
	}
	awayFromAppData(t)
	home, _ := os.UserHomeDir()
	sys := os.Getenv("SystemRoot")
	vol := strings.TrimSuffix(filepath.VolumeName(sys), ":")
	for _, p := range []string{
		`\\?\` + home,
		`\\?\` + sys,
		`\\.\` + sys,
		`\\localhost\` + vol + `$` + strings.TrimPrefix(sys, filepath.VolumeName(sys)),
		sys + `.`,
		filepath.Join(sys, "System32") + ` `,
	} {
		if err := checkFolder(p, nil, nil); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}
