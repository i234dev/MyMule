package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// mymule.conf holds the api_key: other users of the machine must not
// be able to read it, nor the other private files.
func TestPrivateFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	if _, err := LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(home, SettingsFile)); st.Mode().Perm() != 0o600 {
		t.Fatalf("new mymule.conf is %v, want 0600", st.Mode().Perm())
	}

	// Files left world-readable by an older version are fixed on start.
	for _, name := range []string{"known.json", "mymule.log"} {
		os.WriteFile(filepath.Join(home, name), []byte("{}"), 0o644)
	}
	for _, name := range []string{SettingsFile, "known.json", "mymule.log"} {
		os.Chmod(filepath.Join(home, name), 0o644) // keep the content, loosen the mode
	}
	os.WriteFile(filepath.Join(home, "server.met"), []byte("x"), 0o644) // public data
	if _, err := LoadSettings(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{SettingsFile, "known.json", "mymule.log"} {
		if st, _ := os.Stat(filepath.Join(home, name)); st.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v after start, want 0600", name, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(filepath.Join(home, "server.met")); st.Mode().Perm() != 0o644 {
		t.Errorf("server.met changed to %v; only private files should be", st.Mode().Perm())
	}
}

// A data folder MyMule creates itself is private.
func TestNewDataFolderIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	home := filepath.Join(t.TempDir(), "new")
	t.Setenv("MYMULE_HOME", home)
	if _, err := Dir(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(home); st.Mode().Perm() != 0o700 {
		t.Fatalf("new data folder is %v, want 0700", st.Mode().Perm())
	}
}

func TestLocationWarning(t *testing.T) {
	profile := `C:\Users\Someone`
	for dir, warn := range map[string]bool{
		`C:\MyMule`:                         true,
		`D:\Apps\MyMule`:                    true,
		`C:\Users\Someone\Documents\MyMule`: false,
		`c:\users\someone\MyMule`:           false,
		`C:\Users\SomeoneElse\MyMule`:       true,
	} {
		if got := locationWarning("windows", dir, profile) != ""; got != warn {
			t.Errorf("%s: warning=%v, want %v", dir, got, warn)
		}
	}
	if locationWarning("linux", "/opt/mymule", "") != "" {
		t.Error("warning outside Windows")
	}
}

// TightenDir removes other users' write access from MyMule's own
// temporary folder, but leaves alone /tmp-like folders (sticky bit) and a
// folder holding the downloads folder, which other programs may need.
func TestTightenDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits on Windows")
	}
	base := t.TempDir()
	own := filepath.Join(base, "temp")
	os.Mkdir(own, 0o777)
	os.Chmod(own, 0o777)
	if !TightenDir(own) {
		t.Fatal("open temporary folder not tightened")
	}
	if st, _ := os.Stat(own); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v, want 0755 (only write access removed)", st.Mode().Perm())
	}
	sticky := filepath.Join(base, "tmp")
	os.Mkdir(sticky, 0o777)
	os.Chmod(sticky, 0o777|os.ModeSticky)
	if TightenDir(sticky) {
		t.Fatal("a sticky folder like /tmp was changed")
	}
	shared := filepath.Join(base, "data")
	os.MkdirAll(filepath.Join(shared, "downloads"), 0o777)
	os.Chmod(shared, 0o775)
	if TightenDir(shared, filepath.Join(shared, "downloads")) {
		t.Fatal("a folder holding the downloads folder was changed")
	}
}
