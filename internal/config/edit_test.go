package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Settings changed from the API are written in place, comments and the
// rest of the file kept; a value that does not read, a locked setting or
// an unknown one changes nothing.
func TestUpdateSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	p := filepath.Join(home, SettingsFile)
	orig := "# mine\nport = 4662\r\n# keep\nincoming_dir = old\nupload_limit = 10\nupload_limit = 20\napi_key = example-key-0123456789\n"
	os.WriteFile(p, []byte(orig), 0o600)

	if err := UpdateSettings(map[string]string{"port": "5000", "download_dir": "/data/done", "upload_limit": "300", "nick": "Example Nick"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	for _, want := range []string{"# mine\nport = 5000\r\n", "# keep\ndownload_dir = /data/done\n", "upload_limit = 300\nupload_limit = 300\n", "nick = Example Nick\n", "api_key = example-key-0123456789\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	list, err := ReadSettingsForEdit()
	if err != nil {
		t.Fatal(err)
	}
	for _, si := range list {
		switch si.Key {
		case "port":
			if si.Value != "5000" || si.Default != "4662" || si.Kind != "int" {
				t.Errorf("port %+v", si)
			}
		case "api_key":
			if si.Value != "" || !si.Locked {
				t.Error("api key sent out, or not locked")
			}
		case "upnp":
			if si.Kind != "bool" || si.Value != "true" {
				t.Errorf("upnp %+v", si)
			}
		}
	}

	for name, vals := range map[string]map[string]string{
		"bad value":  {"port": "99999"},
		"locked":     {"api_listen": "0.0.0.0:4711"},
		"unknown":    {"nonsense": "1"},
		"multi-line": {"nick": "a\nweb_ui = true"},
		"bad first":  {"nick": "fine", "kad_port": "0"},
	} {
		err := UpdateSettings(vals)
		if err == nil {
			t.Errorf("%s: written", name)
		}
		if name == "locked" && !errors.Is(err, ErrLockedSetting) {
			t.Errorf("locked: %v", err)
		}
	}
	if b2, _ := os.ReadFile(p); string(b2) != got {
		t.Fatal("file changed by a refused update")
	}
}
