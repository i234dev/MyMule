package prefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// awayFromAppData points the Windows per-user application folders, which
// are refused and which also hold the test temporary folders, elsewhere.
func awayFromAppData(t *testing.T) {
	t.Helper()
	other := filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), "no-such-appdata-for-tests")
	for _, v := range []string{"APPDATA", "LOCALAPPDATA"} {
		t.Setenv(v, filepath.Join(other, v))
	}
}

func TestLibraryFolderRules(t *testing.T) {
	awayFromAppData(t)
	base := t.TempDir() // the data folder
	tmp := filepath.Join(t.TempDir(), "mymule-temp")
	s := Open(filepath.Join(base, "preferences.json"), base)
	s.Protect(tmp)
	home, _ := os.UserHomeDir()
	scratch := t.TempDir()

	bad := map[string]string{
		"drive root":            filepath.VolumeName(scratch) + string(filepath.Separator),
		"home":                  home,
		"above home":            filepath.Dir(home),
		"data folder":           base,
		"above the data folder": filepath.Dir(base),
		"temporary folder":      tmp,
		"inside temporary":      filepath.Join(tmp, "movies"),
		"hidden folder":         filepath.Join(scratch, ".config", "autostart"),
	}
	if runtime.GOOS == "windows" {
		bad["Windows folder"] = os.Getenv("SystemRoot")
		appdata := filepath.Join(t.TempDir(), "Roaming")
		t.Setenv("APPDATA", appdata)
		bad["Startup folder"] = filepath.Join(appdata, `Microsoft\Windows\Start Menu\Programs\Startup`)
	} else {
		bad["/etc"] = "/etc"
		bad["inside /usr"] = "/usr/local/share/movies"
		bad["holding /var/lib"] = "/var"
		bad["holding every user's folder"] = "/home"
	}
	for what, dir := range bad {
		p := s.Get()
		p.Library.MoviesDir = dir
		if err := s.Set(p); err == nil {
			t.Errorf("%s (%s) accepted as the movies folder", what, dir)
		}
	}
	if s.Get().Library.MoviesDir != "" {
		t.Fatalf("a refused folder was saved: %q", s.Get().Library.MoviesDir)
	}
	if _, err := os.Stat(filepath.Join(scratch, ".config")); err == nil {
		t.Fatal("a refused folder was created")
	}

	for _, good := range []string{filepath.Join(scratch, "Movies"), "Movies" /* inside the data folder */} {
		p := s.Get()
		p.Library.MoviesDir = good
		if err := s.Set(p); err != nil {
			t.Errorf("%s refused: %v", good, err)
		}
	}
}

// A link to a refused folder is refused too.
func TestLibraryFolderLink(t *testing.T) {
	awayFromAppData(t)
	target := "/etc"
	if runtime.GOOS == "windows" {
		target = os.Getenv("SystemRoot")
	}
	base := t.TempDir()
	link := filepath.Join(t.TempDir(), "Movies")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create links here: %v", err)
	}
	s := Open(filepath.Join(base, "preferences.json"), base)
	p := s.Get()
	p.Library.MoviesDir = link
	if err := s.Set(p); err == nil {
		t.Fatalf("a link to %s was accepted", target)
	}
}

// Folders saved before these rules existed (or by hand) are not used.
func TestLibraryIgnoresSavedBadFolder(t *testing.T) {
	awayFromAppData(t)
	base := t.TempDir()
	home, _ := os.UserHomeDir()
	good := filepath.Join(t.TempDir(), "Series")
	js := `{"library":{"movies_dir":` + quote(home) + `,"series_dir":` + quote(good) + `,"share":true}}`
	if err := os.WriteFile(filepath.Join(base, "preferences.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Open(filepath.Join(base, "preferences.json"), base).Library()
	if l.MoviesDir != "" {
		t.Fatalf("saved home folder still used as movies folder: %q", l.MoviesDir)
	}
	if l.SeriesDir != good {
		t.Fatalf("good series folder lost: %q", l.SeriesDir)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Preferences saved before the "organize" switch: organising was on
// whenever a folder was set, and stays that way after the upgrade.
func TestOrganizeSwitchUpgrade(t *testing.T) {
	awayFromAppData(t)
	folder := filepath.Join(t.TempDir(), "Movies")
	for name, c := range map[string]struct {
		json string
		want bool
	}{
		"old, with a folder":     {`{"library":{"movies_dir":` + quote(folder) + `}}`, true},
		"old, without folders":   {`{"library":{"movies_dir":""}}`, false},
		"new, switched off":      {`{"library":{"organize":false,"movies_dir":` + quote(folder) + `}}`, false},
		"new, on without folder": {`{"library":{"organize":true}}`, true},
	} {
		base := t.TempDir()
		os.WriteFile(filepath.Join(base, "preferences.json"), []byte(c.json), 0o600)
		if got := Open(filepath.Join(base, "preferences.json"), base).Get().Library.Organize; got != c.want {
			t.Errorf("%s: organize=%v, want %v", name, got, c.want)
		}
	}
}

// A folder MyMule cannot create says who MyMule runs as, which is what
// the user has to fix.
func TestFolderErrorNamesTheUser(t *testing.T) {
	awayFromAppData(t)
	base := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	s := Open(filepath.Join(base, "preferences.json"), base)
	p := s.Get()
	p.Library.MoviesDir = filepath.Join(blocker, "Movies")
	err := s.Set(p)
	if err == nil || !strings.Contains(err.Error(), "MyMule") || !strings.Contains(err.Error(), "can't create it") {
		t.Fatalf("error = %v", err)
	}
}
