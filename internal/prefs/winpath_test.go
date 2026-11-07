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
		if windowsPathProblem(p, []string{"nas"}) == nil {
			t.Errorf("%s accepted", p)
		}
	}
	for _, p := range []string{`D:\Movies`, `C:\Users\someone\Videos\Series`, `\\nas\media\Movies`, `\\192.0.2.10\media`, `D:\.\Movies\..\Films`} {
		if err := windowsPathProblem(p, []string{"NAS", "192.0.2.10"}); err != nil {
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
		if err := checkFolder(p, nil, nil, nil); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

// Opening a network folder sends the Windows login hash to that
// computer, so only the computers in network_hosts are accepted.
func TestNetworkFoldersOnlyFromListedHosts(t *testing.T) {
	for _, p := range []string{`\\attacker\s\m`, `\\attacker@SSL@443\DavWWWRoot\x`, `\\attacker@80\x\y`, `//attacker/s`, `\\nas.example.test\media`} {
		if windowsPathProblem(p, []string{"nas"}) == nil {
			t.Errorf("%s accepted", p)
		}
	}
	if windowsPathProblem(`\\nas\media\Movies`, nil) == nil {
		t.Error("network folder accepted with no network_hosts")
	}
	for _, p := range []string{`\\nas\media`, `\\NAS\media\Movies`, `\\nas@SSL@443\DavWWWRoot\x`, `//nas/media`} {
		if err := windowsPathProblem(p, []string{"nas"}); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
}
