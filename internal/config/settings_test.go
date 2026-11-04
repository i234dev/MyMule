package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsCreatedWithDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != 4662 || s.Nick != "MyMule" || s.MaxConns != 40 || s.Server != "" {
		t.Fatalf("defaults: %+v", s)
	}
	if s.TempDir != filepath.Join(home, "temp") || s.DownloadDir != filepath.Join(home, "downloads") {
		t.Fatalf("dirs: %+v", s)
	}
	// The generated file must parse back to the same values.
	s2, err := LoadSettings()
	if err != nil || s2 != s {
		t.Fatalf("reload: %+v %v", s2, err)
	}
}

func TestSettingsParse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	abs := filepath.Join(t.TempDir(), "done")
	conf := "# comment\nport = 4650\nserver = 176.123.5.89:4725\nnick = \"Sergio\"\n" +
		"temp_dir = tmp\ndownload_dir = " + abs + "\nmax_conns = 10\n"
	os.WriteFile(filepath.Join(home, SettingsFile), []byte(conf), 0o644)
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Port: 4650, Server: "176.123.5.89:4725", Nick: "Sergio",
		TempDir: filepath.Join(home, "tmp"), DownloadDir: abs, MaxConns: 10}
	if s != want {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
}

func TestSettingsErrors(t *testing.T) {
	for _, bad := range []string{"port = 99999", "max_conns = 0", "portt = 1", "just text"} {
		var s Settings
		if err := parseSettings(bad, &s); err == nil {
			t.Errorf("%q accepted", bad)
		}
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
}
