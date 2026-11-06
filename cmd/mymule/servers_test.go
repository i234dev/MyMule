package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mymule/internal/download"
	"mymule/internal/prefs"
	"mymule/internal/proto"
	"mymule/internal/server"
)

func TestDefaultServerListUsesHTTPS(t *testing.T) {
	for _, u := range defaultServerMetURLs {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("default server list %s is not HTTPS", u)
		}
	}
}

// serverMet builds a server.met holding the given ip:port pairs.
func serverMet(entries ...server.Info) []byte {
	var w proto.Writer
	w.U8(0x0E)
	w.U32(uint32(len(entries)))
	for _, s := range entries {
		w.U32(server.IPToU32(s.IP))
		w.U16(s.Port)
		w.U32(0)
	}
	return w.B
}

func info(ip string, port uint16) server.Info {
	return server.Info{IP: net.ParseIP(ip).To4(), Port: port}
}

func TestFetchedServerListIsFiltered(t *testing.T) {
	met := serverMet(
		info("11.0.0.1", 4661),    // fine
		info("11.0.0.1", 4661),    // repeated
		info("127.0.0.1", 4661),   // this machine
		info("192.168.1.1", 4661), // the user's LAN
		info("10.0.0.1", 4661),    // private
		info("11.0.0.2", 0),       // no port
		info("224.0.0.1", 4661),   // multicast
		info("11.0.0.3", 4242),    // fine
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(met) }))
	defer srv.Close()
	old := serverListClient
	serverListClient = &http.Client{Transport: srv.Client().Transport, CheckRedirect: old.CheckRedirect}
	defer func() { serverListClient = old }()

	list, err := fetchServerMet(srv.URL + "/server.met")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, s.Addr())
	}
	if strings.Join(got, " ") != "11.0.0.1:4661 11.0.0.3:4242" {
		t.Fatalf("kept %v", got)
	}

	var many []server.Info
	for i := 0; i < maxFetchedServers+50; i++ {
		many = append(many, server.Info{IP: net.IPv4(11, 1, byte(i>>8), byte(i)), Port: 4661})
	}
	if n := len(usableServers(many)); n != maxFetchedServers {
		t.Fatalf("kept %d servers, want %d", n, maxFetchedServers)
	}
}

// A redirect from HTTPS to plain HTTP is refused.
func TestServerListRefusesHTTPDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(serverMet(info("11.0.0.9", 4661)))
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/server.met", http.StatusFound)
	}))
	defer tlsSrv.Close()
	old := serverListClient
	serverListClient = &http.Client{Transport: tlsSrv.Client().Transport, CheckRedirect: old.CheckRedirect}
	defer func() { serverListClient = old }()

	if _, err := fetchServerMet(tlsSrv.URL + "/server.met"); err == nil {
		t.Fatal("followed a redirect from HTTPS to HTTP")
	}
}

// Slow clients must not be able to hold API connections open for ever.
func TestHTTPServerTimeouts(t *testing.T) {
	hs := newHTTPServer(http.NotFoundHandler())
	if hs.ReadHeaderTimeout <= 0 || hs.ReadTimeout <= 0 || hs.WriteTimeout <= 0 || hs.IdleTimeout <= 0 {
		t.Fatalf("missing timeouts: %+v", hs)
	}
	if hs.WriteTimeout < 60*time.Second {
		t.Fatalf("write timeout %v leaves no room for a 45 s search", hs.WriteTimeout)
	}
}

func TestRotateLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mymule.log")
	os.WriteFile(p, make([]byte, 100), 0o600)
	rotateLog(p, 1000)
	if _, err := os.Stat(p + ".1"); err == nil {
		t.Fatal("small log rotated")
	}
	os.WriteFile(p, make([]byte, 2000), 0o600)
	rotateLog(p, 1000)
	if st, err := os.Stat(p + ".1"); err != nil || st.Size() != 2000 {
		t.Fatal("big log not kept as .1")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("old log still in place")
	}
}

// A server that stops answering searches: kept (with a warning) when the
// user chose it, otherwise replaced by the best other known server.
func TestSilentServerPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYMULE_HOME", home)
	list := []server.Info{info("11.0.0.1", 4661), info("11.0.0.2", 4661)}
	list[1].Users = 5000 // the better one
	if err := saveServers(list); err != nil {
		t.Fatal(err)
	}
	newEngine := func(preferred string) *engine {
		ps := prefs.Open(filepath.Join(t.TempDir(), "preferences.json"), t.TempDir())
		if preferred != "" {
			p := ps.Get()
			p.Server = preferred
			if err := ps.Set(p); err != nil {
				t.Fatal(err)
			}
		}
		return &engine{Manager: download.NewManager(download.Config{}), prefs: ps}
	}

	e := newEngine("11.0.0.1:4661")
	e.silentServer("11.0.0.1:4661", "")
	if e.next != "" {
		t.Fatalf("switched away from the user's chosen server, to %s", e.next)
	}
	e = newEngine("")
	e.silentServer("11.0.0.2:4661", "11.0.0.2:4661") // chosen in mymule.conf
	if e.next != "" {
		t.Fatalf("switched away from the server set in mymule.conf, to %s", e.next)
	}
	e = newEngine("")
	e.silentServer("11.0.0.2:4661", "")
	if e.next != "11.0.0.1:4661" {
		t.Fatalf("switched to %q, want the other server", e.next)
	}
}
