package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
