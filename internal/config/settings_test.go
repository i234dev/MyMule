package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSettingsCreatedWithDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != 4662 || s.Nick != "MyMule" || s.MaxConns != 40 || s.Server != "" ||
		!s.Sharing || !s.ShareDownloads || s.UploadSlots != 3 || s.UploadLimit != 100 || s.SharedDirs != nil {
		t.Fatalf("defaults: %+v", s)
	}
	if s.TempDir != filepath.Join(home, "temp") || s.DownloadDir != filepath.Join(home, "downloads") {
		t.Fatalf("dirs: %+v", s)
	}
	// The generated file must parse back to the same values, unchanged.
	before, _ := os.ReadFile(filepath.Join(home, SettingsFile))
	s2, err := LoadSettings()
	if err != nil || !reflect.DeepEqual(s2, s) {
		t.Fatalf("reload: %+v %v", s2, err)
	}
	if after, _ := os.ReadFile(filepath.Join(home, SettingsFile)); string(after) != string(before) {
		t.Fatal("a fresh config was modified on reload")
	}
}

func TestSettingsParse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	abs := filepath.Join(t.TempDir(), "done")
	lines := []string{
		"# comment",
		"port = 4650",
		"server = 176.123.5.89:4725",
		`nick = "Sergio"`,
		"temp_dir = tmp",
		"download_dir = " + abs,
		"max_conns = 10",
		"sharing = false",
		"share_downloads = false",
		"shared_dirs = isos; " + abs,
		"upload_slots = 5",
		"upload_limit = 0",
	}
	os.WriteFile(filepath.Join(home, SettingsFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Port: 4650, Server: "176.123.5.89:4725", Nick: "Sergio",
		TempDir: filepath.Join(home, "tmp"), DownloadDir: abs, MaxConns: 10,
		Sharing: false, ShareDownloads: false, SharedDirs: []string{filepath.Join(home, "isos"), abs},
		UploadSlots: 5, UploadLimit: 0}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
}

func TestSettingsErrors(t *testing.T) {
	for _, bad := range []string{"port = 99999", "max_conns = 0", "portt = 1", "just text",
		"share_downloads = maybe", "upload_slots = 0", "upload_limit = -1"} {
		var s Settings
		if _, err := parseSettings(bad, &s); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// An old config gets the new settings appended, keeping its own values.
func TestSettingsUpgrade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	p := filepath.Join(home, SettingsFile)
	old := "# mine\nport = 4650\n"
	os.WriteFile(p, []byte(old), 0o644)
	s, err := LoadSettings()
	if err != nil || s.Port != 4650 || s.UploadLimit != 100 {
		t.Fatalf("%+v %v", s, err)
	}
	data, _ := os.ReadFile(p)
	text := string(data)
	if !strings.HasPrefix(text, old) || !strings.Contains(text, "upload_limit = 100") ||
		strings.Count(text, "\nport = ") != 1 {
		t.Fatalf("upgraded file:\n%s", text)
	}
	LoadSettings()
	if again, _ := os.ReadFile(p); string(again) != text {
		t.Fatal("settings appended twice")
	}
}

func TestImportLegacy(t *testing.T) {
	legacyBase := t.TempDir()
	t.Setenv("APPDATA", legacyBase)         // Windows
	t.Setenv("XDG_CONFIG_HOME", legacyBase) // Linux
	t.Setenv("HOME", legacyBase)            // macOS (Library/Application Support)
	old := LegacyDir()
	os.MkdirAll(filepath.Join(old, "temp"), 0o755)
	os.WriteFile(filepath.Join(old, "userhash.dat"), make([]byte, 16), 0o600)
	os.WriteFile(filepath.Join(old, "server.met"), []byte{0xE0, 0, 0, 0, 0}, 0o644)
	os.WriteFile(filepath.Join(old, "temp", "AB.part"), []byte("data"), 0o644)
	os.WriteFile(filepath.Join(old, "temp", "AB.part.json"), []byte("{}"), 0o644)

	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	tmp := filepath.Join(home, "temp")
	notes := ImportLegacy(tmp)
	if len(notes) == 0 {
		t.Fatal("nothing imported")
	}
	for _, f := range []string{filepath.Join(home, "userhash.dat"), filepath.Join(home, "server.met"),
		filepath.Join(tmp, "AB.part"), filepath.Join(tmp, "AB.part.json")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(old, "temp", "AB.part")); !os.IsNotExist(err) {
		t.Error("part file was copied instead of moved")
	}
	if again := ImportLegacy(tmp); again != nil {
		t.Errorf("second import did something: %v", again)
	}
	// Another fresh MyMule folder must not inherit the same identity.
	t.Setenv("MYMULE_HOME", t.TempDir())
	if other := ImportLegacy(filepath.Join(t.TempDir(), "temp")); other != nil {
		t.Errorf("identity imported twice: %v", other)
	}
}

// Data imported by an older build (copy left behind) is retired on the
// next run.
func TestLegacyIdentityRetired(t *testing.T) {
	legacyBase := t.TempDir()
	t.Setenv("APPDATA", legacyBase)
	t.Setenv("XDG_CONFIG_HOME", legacyBase)
	t.Setenv("HOME", legacyBase)
	old := LegacyDir()
	os.MkdirAll(old, 0o755)
	id := []byte("0123456789abcdef")
	os.WriteFile(filepath.Join(old, "userhash.dat"), id, 0o600)
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	os.WriteFile(filepath.Join(home, "userhash.dat"), id, 0o600)
	ImportLegacy(filepath.Join(home, "temp"))
	if _, err := os.Stat(filepath.Join(old, "userhash.dat")); !os.IsNotExist(err) {
		t.Fatal("old identity not retired")
	}
}
