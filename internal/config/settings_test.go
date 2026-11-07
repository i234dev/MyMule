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
		!s.Sharing || !s.ShareDownloads || s.UploadSlots != 20 || s.UploadLimit != 1000 || s.SharedDirs != nil ||
		s.APIListen != "127.0.0.1:4711" || len(s.APIKey) != 32 {
		t.Fatalf("defaults: %+v", s)
	}
	// Every installation gets its own random key.
	t.Setenv("MYMULE_HOME", t.TempDir())
	if other, _ := LoadSettings(); other.APIKey == s.APIKey {
		t.Fatal("two installations got the same API key")
	}
	t.Setenv("MYMULE_HOME", home)
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
		"port = 12345",
		"kad = false",
		"kad_port = 5000",
		"kad_nodes_url = https://example.org/nodes.dat",
		"server = 192.0.2.10:4661",
		`nick = "tester"`,
		"temp_dir = tmp",
		"download_dir = " + abs,
		"max_conns = 10",
		"sharing = false",
		"share_downloads = false",
		"shared_dirs = isos; " + abs,
		"upload_slots = 5",
		"upload_limit = 0",
		"api_listen = 0.0.0.0:8080",
		"api_key = example-key-0123456789",
		"web_ui = true",
		"tmdb_api_key = off",
		"arr_support = true",
		"search_languages = ES, pt-br",
		`network_hosts = NAS, \\192.0.2.20\, `,
	}
	os.WriteFile(filepath.Join(home, SettingsFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Port: 12345, Kad: false, KadPort: 5000, KadNodesURL: "https://example.org/nodes.dat", Server: "192.0.2.10:4661", Nick: "tester",
		TempDir: filepath.Join(home, "tmp"), DownloadDir: abs, MaxConns: 10,
		Sharing: false, ShareDownloads: false, SharedDirs: []string{filepath.Join(home, "isos"), abs},
		UploadSlots: 5, UploadLimit: 0, APIListen: "0.0.0.0:8080", APIKey: "example-key-0123456789",
		WebUI: true, TMDBKey: "off", ArrSupport: true, SearchLanguages: []string{"es", "pt-br"},
		NetworkHosts: []string{"nas", "192.0.2.20"}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("got %+v\nwant %+v", s, want)
	}
}

func TestSettingsErrors(t *testing.T) {
	for _, bad := range []string{"port = 99999", "max_conns = 0", "portt = 1", "just text",
		"share_downloads = maybe", "upload_slots = 0", "upload_limit = -1",
		"api_listen = 4711", "api_key = short", "search_languages = spanish", "arr_support = yes please", "web_ui = on", "tmdb_api_key = secret"} {
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
	old := "# mine\nport = 12345\n"
	os.WriteFile(p, []byte(old), 0o644)
	s, err := LoadSettings()
	if err != nil || s.Port != 12345 || s.UploadLimit != 1000 || len(s.APIKey) != 32 {
		t.Fatalf("%+v %v", s, err)
	}
	data, _ := os.ReadFile(p)
	text := string(data)
	if !strings.HasPrefix(text, old) || !strings.Contains(text, "upload_limit = 1000") ||
		!strings.Contains(text, "api_key = "+s.APIKey) || strings.Count(text, "\nport = ") != 1 {
		t.Fatalf("upgraded file:\n%s", text)
	}
	s2, _ := LoadSettings()
	if again, _ := os.ReadFile(p); string(again) != text || s2.APIKey != s.APIKey {
		t.Fatal("settings appended twice or the key changed")
	}
}

// An empty api_key line is filled with a new random key, in place.
func TestEmptyAPIKeyIsGenerated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	p := filepath.Join(home, SettingsFile)
	os.WriteFile(p, []byte("api_key =\n"), 0o644)
	s, err := LoadSettings()
	if err != nil || len(s.APIKey) != 32 {
		t.Fatalf("%+v %v", s, err)
	}
	data, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(data), "api_key = "+s.APIKey+"\n") {
		t.Fatalf("key not written in place:\n%s", data)
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

func TestKadSettings(t *testing.T) {
	d := Defaults()
	if !d.Kad || d.KadPort != 4672 || d.KadNodesURL != DefaultKadNodesURL {
		t.Fatalf("defaults %v %d %q", d.Kad, d.KadPort, d.KadNodesURL)
	}
	for _, bad := range []string{"kad = maybe", "kad_port = 0", "kad_port = 70000", "kad_nodes_url = ftp://x/nodes.dat", "kad_nodes_url = file:///etc/passwd", "kad_nodes_url = nodes.dat"} {
		s := Defaults()
		if _, err := parseSettings(bad+"\n", &s); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	s := Defaults()
	if _, err := parseSettings("kad_nodes_url =\n", &s); err != nil || s.KadNodesURL != "" {
		t.Fatalf("empty URL: %v %q", err, s.KadNodesURL)
	}
}
