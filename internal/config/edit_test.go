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
		// Quotes and spaces are taken off when the file is read: the
		// folder used is the one checked.
		{"download_dir": `"` + other + `"`},
		{"temp_dir": ` "` + other + `" `},
		{"shared_dirs": `"` + other + `"`},
	} {
		if err := UpdateSettings(vals, check); err == nil {
			t.Errorf("%v: written", vals)
		}
	}
	if err := UpdateSettings(map[string]string{"download_dir": other}, nil); err == nil {
		t.Error("folder changed without a check")
	}
	// Folders already in mymule.conf are not checked again; a new one is,
	// inside the data folder too.
	checked = nil
	record := func(abs string) error {
		checked = append(checked, abs)
		return nil
	}
	if err := UpdateSettings(map[string]string{"temp_dir": "temp", "shared_dirs": "videos;" + outside, "download_dir": outside}, record); err != nil {
		t.Fatal(err)
	}
	if len(checked) != 1 || checked[0] != filepath.Join(home, "videos") {
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
		case "tmdb_api_key":
			if si.Value != "" || !si.Secret {
				t.Errorf("%+v", si)
			}
		}
	}
}

// The media server token goes only where it was sent with: a new address
// needs the token again.
func TestMediaServerTokenFollows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	p := filepath.Join(home, SettingsFile)
	os.WriteFile(p, []byte("media_server = jellyfin\nmedia_server_url = http://192.0.2.10:8096\nmedia_server_token = example-token\n"), 0o600)
	for _, vals := range []map[string]string{
		{"media_server_url": "http://203.0.113.66:8096"},
		{"media_server": "plex"},
	} {
		if err := UpdateSettings(vals, okFolder); err == nil {
			t.Errorf("%v: changed without the token", vals)
		}
	}
	if err := UpdateSettings(map[string]string{"media_server_url": "http://192.0.2.11:8096", "media_server_token": "new-token"}, okFolder); err != nil {
		t.Fatal(err)
	}
	if err := UpdateSettings(map[string]string{"media_server_url": "http://192.0.2.11:8096"}, okFolder); err != nil {
		t.Fatal("unchanged address refused:", err)
	}
}

// With the media server and its token in the environment (Docker) and the
// address in the file, the address cannot change from the API: the token
// would go to the new one.
func TestMediaServerTokenInEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	t.Setenv("MYMULE_MEDIA_SERVER", "jellyfin")
	t.Setenv("MYMULE_MEDIA_SERVER_TOKEN", "example-token")
	p := filepath.Join(home, SettingsFile)
	os.WriteFile(p, []byte("media_server_url = http://192.0.2.10:8096\n"), 0o600)
	for _, vals := range []map[string]string{
		{"media_server_url": "http://203.0.113.66:8096"},
		{"media_server_url": "http://203.0.113.66:8096", "media_server_token": "anything"},
	} {
		if err := UpdateSettings(vals, okFolder); err == nil {
			t.Errorf("%v: changed with the token in the environment", vals)
		}
	}
	if err := UpdateSettings(map[string]string{"upload_limit": "300"}, okFolder); err != nil {
		t.Fatal("unrelated change refused:", err)
	}
	// The token only in the environment, the server and address in the file.
	t.Setenv("MYMULE_MEDIA_SERVER", "")
	os.Unsetenv("MYMULE_MEDIA_SERVER")
	os.WriteFile(p, []byte("media_server = jellyfin\nmedia_server_url = http://192.0.2.10:8096\n"), 0o600)
	if err := UpdateSettings(map[string]string{"media_server": "emby"}, okFolder); err == nil {
		t.Error("media server changed with the token in the environment")
	}
}

// The peer port (opened on the router) can never be the API's.
func TestPortIsNotTheAPIs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	os.WriteFile(filepath.Join(home, SettingsFile), []byte("api_listen = 127.0.0.1:4711\n"), 0o600)
	if err := UpdateSettings(map[string]string{"port": "4711", "upnp": "true"}, okFolder); err == nil {
		t.Fatal("port set to the API's")
	}
	if err := UpdateSettings(map[string]string{"port": "4663"}, okFolder); err != nil {
		t.Fatal(err)
	}
}

// A new folder written inside the data folder still follows the folder
// rules: it may be a link out of it, or a hidden folder. Refused, nothing
// is written.
func TestUpdateSettingsInsideDataFolderChecked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	p := filepath.Join(home, SettingsFile)
	os.WriteFile(p, []byte("# empty\n"), 0o600)
	before, _ := os.ReadFile(p)
	var checked []string
	refuse := func(abs string) error {
		checked = append(checked, abs)
		return errors.New("refused by the folder rules")
	}
	for _, d := range []string{"escape", ".hidden-startup"} {
		checked = nil
		if err := UpdateSettings(map[string]string{"download_dir": d}, refuse); err == nil {
			t.Errorf("%s: written", d)
		}
		if len(checked) != 1 || checked[0] != filepath.Join(home, d) {
			t.Errorf("%s: checked %v", d, checked)
		}
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("mymule.conf changed")
	}
}
