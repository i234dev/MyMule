package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	if err := UpdateSettings(map[string]string{"port": "5000", "download_dir": "/data/done", "upload_limit": "300", "nick": "Example Nick"}, okFolder); err != nil {
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
		err := UpdateSettings(vals, okFolder)
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

func okFolder(string) error { return nil }

// A new folder outside the data folder must pass the check; one inside
// it, or already in mymule.conf, does not need to.
func TestUpdateSettingsFolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	p := filepath.Join(home, SettingsFile)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(p, []byte("download_dir = "+outside+"\n"), 0o600)
	var checked []string
	check := func(abs string) error {
		checked = append(checked, abs)
		return errors.New("not this one")
	}
	other := filepath.Join(t.TempDir(), "other")
	for _, vals := range []map[string]string{
		{"shared_dirs": "videos; " + other},
		{"temp_dir": other},
		{"download_dir": filepath.Join("..", "escape")},
	} {
		if err := UpdateSettings(vals, check); err == nil {
			t.Errorf("%v: written", vals)
		}
	}
	if err := UpdateSettings(map[string]string{"download_dir": other}, nil); err == nil {
		t.Error("folder changed without a check")
	}
	checked = nil
	if err := UpdateSettings(map[string]string{"temp_dir": "temp", "shared_dirs": "videos;" + outside, "download_dir": outside}, check); err != nil {
		t.Fatal(err)
	}
	if len(checked) != 0 {
		t.Fatalf("checked %v", checked)
	}
	if err := UpdateSettings(map[string]string{"shared_dirs": "."}, check); err == nil {
		t.Error("the data folder itself shared")
	}
}

// Changes made at the same time are all kept.
func TestUpdateSettingsAtOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	p := filepath.Join(home, SettingsFile)
	os.WriteFile(p, []byte("# empty\n"), 0o600)
	vals := map[string]string{"port": "5001", "kad_port": "5002", "upload_limit": "33", "download_limit": "44", "upload_slots": "7", "max_conns": "123"}
	var wg sync.WaitGroup
	for k, v := range vals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := UpdateSettings(map[string]string{k: v}, okFolder); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(p)
	for k, v := range vals {
		if !strings.Contains(string(b), "\n"+k+" = "+v+"\n") {
			t.Errorf("%s lost:\n%s", k, b)
		}
	}
}

// Tokens are not sent out; whether one is set is.
func TestSecretSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	os.WriteFile(filepath.Join(home, SettingsFile), []byte("api_key = example-key-0123456789\nnotify_urls = ntfy://example-topic\n"), 0o600)
	list, err := ReadSettingsForEdit()
	if err != nil {
		t.Fatal(err)
	}
	for _, si := range list {
		switch si.Key {
		case "notify_urls":
			if si.Value != "" || !si.Secret || !si.Set {
				t.Errorf("%+v", si)
			}
		case "media_server_token":
			if si.Value != "" || !si.Secret || si.Set {
				t.Errorf("%+v", si)
			}
		}
	}
}
