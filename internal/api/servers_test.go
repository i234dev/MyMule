package api

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mymule/internal/download"
	"mymule/internal/library"
	"mymule/internal/prefs"
	"mymule/internal/server"
)

type fakeServers struct {
	connected []string
	updated   int
}

func (f *fakeServers) Servers() ([]server.Info, error) {
	return []server.Info{
		{IP: net.ParseIP("192.0.2.10"), Port: 4661, Name: "Example Server A", Users: 54000, Files: 26000000},
		{IP: net.ParseIP("198.51.100.20"), Port: 4232, Name: "Example Server B", Users: 900, Files: 120000, Fails: 2},
	}, nil
}
func (f *fakeServers) ConnectTo(addr string)                        { f.connected = append(f.connected, addr) }
func (f *fakeServers) UpdateServers() (total, added int, err error) { f.updated++; return 3, 1, nil }

func TestServersAPI(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "temp")
	os.MkdirAll(tmp, 0o755)
	m := download.NewManager(download.Config{TempDir: tmp})
	rest := NewREST(m, restKey, "test")
	data := t.TempDir()
	ps := prefs.Open(filepath.Join(data, "preferences.json"), data)
	rest.EnableLibrary(ps, library.OpenStore(filepath.Join(data, "library.json")), nil)
	fs := &fakeServers{}
	rest.EnableServers(fs)
	srv := httptest.NewServer(rest)
	defer srv.Close()

	var list struct {
		Servers   []RServerInfo `json:"servers"`
		Current   string        `json:"current"`
		Preferred string        `json:"preferred"`
	}
	if code := call(t, srv, "GET", "/api/v1/servers", "", "", &list); code != 200 || len(list.Servers) != 2 ||
		list.Servers[0].Address != "192.0.2.10:4661" || list.Servers[0].Users != 54000 || list.Current != "" {
		t.Fatalf("list: %d %+v", code, list)
	}

	if code := call(t, srv, "POST", "/api/v1/servers/connect", "application/json", `{"address":"198.51.100.20:4232"}`, nil); code != 200 ||
		len(fs.connected) != 1 || fs.connected[0] != "198.51.100.20:4232" {
		t.Fatalf("connect: %d %v", code, fs.connected)
	}
	// Only known servers: the API can't be used to connect anywhere.
	for _, bad := range []string{`{"address":"203.0.113.5:4661"}`, `{"address":"example.org:4661"}`, `{"address":""}`} {
		if code := call(t, srv, "POST", "/api/v1/servers/connect", "application/json", bad, nil); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}

	if code := call(t, srv, "PUT", "/api/v1/servers/preferred", "application/json", `{"address":"192.0.2.10:4661"}`, nil); code != 200 ||
		ps.Get().Server != "192.0.2.10:4661" || len(fs.connected) != 2 {
		t.Fatalf("preferred: %d %q %v", code, ps.Get().Server, fs.connected)
	}
	call(t, srv, "GET", "/api/v1/servers", "", "", &list)
	if list.Preferred != "192.0.2.10:4661" {
		t.Fatalf("preferred not listed: %+v", list)
	}
	if code := call(t, srv, "PUT", "/api/v1/servers/preferred", "application/json", `{"address":""}`, nil); code != 200 || ps.Get().Server != "" {
		t.Fatalf("back to automatic: %d %q", code, ps.Get().Server)
	}

	var upd map[string]int
	if code := call(t, srv, "POST", "/api/v1/servers/update", "", "", &upd); code != 200 || upd["servers"] != 3 || fs.updated != 1 {
		t.Fatalf("update: %d %+v", code, upd)
	}
}
