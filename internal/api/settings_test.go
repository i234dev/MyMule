package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mymule/internal/config"
	"mymule/internal/download"
	"mymule/internal/portmap"
	"mymule/internal/prefs"
)

// The settings endpoints show mymule.conf and change it; locked settings
// are refused. The status says what is open on the router.
func TestSettingsAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	if _, err := config.LoadSettings(); err != nil { // creates mymule.conf
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	state := &portmap.State{Method: "UPnP", Ports: []portmap.Port{{Proto: "tcp", Port: 4662}}}
	m := download.NewManager(download.Config{TempDir: tmp, Router: func() *portmap.State { return state }})
	rest := NewREST(m, restKey, "test")
	store := prefs.Open(filepath.Join(home, "preferences.json"), home)
	store.Protect(tmp)
	rest.EnableSettings(store.CheckFolder)
	srv := httptest.NewServer(rest)
	defer srv.Close()

	var got struct {
		File     string               `json:"file"`
		Settings []config.SettingInfo `json:"settings"`
	}
	if code := call(t, srv, "GET", "/api/v1/settings", "", "", &got); code != 200 || len(got.Settings) < 20 || !strings.HasSuffix(got.File, config.SettingsFile) {
		t.Fatalf("get: %d %d", code, len(got.Settings))
	}
	if code := call(t, srv, "PUT", "/api/v1/settings", "application/json", `{"values":{"upload_limit":"250"}}`, nil); code != 200 {
		t.Fatalf("put: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(home, config.SettingsFile)); !strings.Contains(string(b), "\nupload_limit = 250\n") {
		t.Fatal("not saved")
	}
	if code := call(t, srv, "PUT", "/api/v1/settings", "application/json", `{"values":{"api_listen":"0.0.0.0:4711"}}`, nil); code != 403 {
		t.Fatalf("locked: %d", code)
	}
	if code := call(t, srv, "PUT", "/api/v1/settings", "application/json", `{"values":{"port":"x"}}`, nil); code != 400 {
		t.Fatalf("bad value: %d", code)
	}
	// Folders follow the library folders' rules: not the user's whole
	// home, not MyMule's temporary folder.
	userHome, _ := os.UserHomeDir()
	for _, d := range []string{userHome, tmp} {
		body, _ := json.Marshal(map[string]any{"values": map[string]string{"shared_dirs": d}})
		if code := call(t, srv, "PUT", "/api/v1/settings", "application/json", string(body), nil); code != 400 {
			t.Fatalf("shared %s: %d", d, code)
		}
	}
	if runtime.GOOS != "windows" { // its temporary folders are in AppData, refused
		ok := filepath.Join(t.TempDir(), "Shared")
		body, _ := json.Marshal(map[string]any{"values": map[string]string{"shared_dirs": ok}})
		if code := call(t, srv, "PUT", "/api/v1/settings", "application/json", string(body), nil); code != 200 {
			t.Fatalf("shared %s: %d", ok, code)
		}
	}

	var st RStatus
	if code := call(t, srv, "GET", "/api/v1/status", "", "", &st); code != 200 || st.Router == nil || st.Router.Method != "UPnP" ||
		st.Router.Summary() != "TCP 4662 open (UPnP)" {
		t.Fatalf("status router: %+v", st.Router)
	}
	state = &portmap.State{Error: "no router answered"}
	if s := RouterInfoOf(state).Summary(); !strings.Contains(s, "no router answered") {
		t.Fatal(s)
	}
	if s := (*RouterInfo)(nil).Summary(); !strings.Contains(s, "off") {
		t.Fatal(s)
	}
}
