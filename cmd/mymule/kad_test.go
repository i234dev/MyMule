package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/server"
)

func TestFWCheckPayload(t *testing.T) {
	got := fwCheckPayload(0x1234, 0xABCD, 0x01020304)
	want := []byte{0x34, 0x12, 0xCD, 0xAB, 0x04, 0x03, 0x02, 0x01}
	if !bytes.Equal(got, want) {
		t.Fatalf("% X", got)
	}
}

type fakeCallbackNode struct{ calls []string }

func (f *fakeCallbackNode) RequestCallback(ip netip.Addr, port uint16, id kad.ID, file [16]byte) error {
	f.calls = append(f.calls, "buddy "+ip.String())
	return nil
}

func (f *fakeCallbackNode) DirectCallback(ip netip.Addr, port uint16) error {
	f.calls = append(f.calls, "direct "+ip.String())
	return nil
}

func TestKadCallbackDispatch(t *testing.T) {
	f := &fakeCallbackNode{}
	b, d := netip.MustParseAddr("11.1.1.1"), netip.MustParseAddr("11.2.2.2")
	for _, typ := range []byte{3, 5, 6, 1, 2} {
		err := kadCallback(f, kad.Source{Type: typ, BuddyIP: b, IP: d}, ed2k.Hash{})
		if (typ == 1 || typ == 2) != (err != nil) {
			t.Fatalf("type %d: %v", typ, err)
		}
	}
	if strings.Join(f.calls, ",") != "buddy 11.1.1.1,buddy 11.1.1.1,direct 11.2.2.2" {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestKadSearchMapping(t *testing.T) {
	q := kadQuery(server.Query{Keywords: "quiet orchard", Type: server.TypeVideo, MinSize: 1, MaxSize: 1 << 40, Extension: "mkv"})
	if q.Keywords != "quiet orchard" || q.Type != "Video" || q.MinSize != 1 || q.MaxSize != 1<<40 || q.Extension != "mkv" {
		t.Fatalf("%+v", q)
	}
	r := kadResult(kad.KeywordResult{Hash: [16]byte{1}, Name: "Quiet Orchard.mkv", Size: 5, Type: "Video", Sources: 3})
	if r.Hash != (ed2k.Hash{1}) || r.Name != "Quiet Orchard.mkv" || r.Size != 5 || r.Type != "Video" || r.Sources != 3 {
		t.Fatalf("%+v", r)
	}
}

func testNode(t *testing.T) *kad.Node {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	n, err := kad.New(kad.Config{Conn: conn, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Clients that give their Kad port become nodes to start from; others
// (no port, Kad version 0 or 1, private address) do not.
func TestKadPeerHello(t *testing.T) {
	n := testNode(t)
	hello := func(port uint16, version byte) peer.Hello {
		return peer.NewHello(ed2k.Hash{1}, "x", 0x05060708, 4662, nil, 0).WithKad(port, version)
	}
	kadPeerHello(n, net.ParseIP("11.1.1.1"), hello(0, 8))
	kadPeerHello(n, net.ParseIP("11.1.1.2"), hello(4672, 1))
	kadPeerHello(n, net.ParseIP("192.168.1.2"), hello(4672, 8))
	if !n.NeedsNodes() {
		t.Fatal("bootstrap node taken from an unsuitable hello")
	}
	kadPeerHello(n, net.ParseIP("11.1.1.3"), hello(4672, 8))
	if n.NeedsNodes() {
		t.Fatal("bootstrap node not taken")
	}
}

func nodesDat(n int) []byte {
	var w proto.Writer
	w.U32(0)
	w.U32(2)
	w.U32(uint32(n))
	for i := range n {
		id := kad.RandomID()
		kad.WriteID(&w, id)
		w.U32(uint32(11<<24 | i<<8 | 1))
		w.U16(4672)
		w.U16(4662)
		w.U8(8)
		w.U32(0)
		w.U32(0)
		w.U8(0)
	}
	return w.B
}

func TestFetchNodes(t *testing.T) {
	// K nodes: one bin holds them all, so none is dropped whatever their
	// random IDs (with more, a full bin that cannot split drops some).
	good := nodesDat(kad.K)
	mux := http.NewServeMux()
	mux.HandleFunc("/nodes.dat", func(w http.ResponseWriter, r *http.Request) { w.Write(good) })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, kad.MaxNodesFile+10)) })
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/nodes.dat", http.StatusFound) })
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		// localhost: the same server, under another host name.
		http.Redirect(w, r, "http://localhost"+r.Host[strings.LastIndex(r.Host, ":"):]+"/nodes.dat", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	n := testNode(t)
	if c, err := fetchNodes(context.Background(), n, srv.URL+"/nodes.dat"); err != nil || c != kad.K {
		t.Fatalf("good file: %d %v", c, err)
	}
	if c, err := fetchNodes(context.Background(), testNode(t), srv.URL+"/same"); err != nil || c != kad.K {
		t.Fatalf("redirect on the same host: %d %v", c, err)
	}
	for _, path := range []string{"/missing", "/huge", "/elsewhere"} {
		if _, err := fetchNodes(context.Background(), testNode(t), srv.URL+path); err == nil {
			t.Errorf("%s: no error", path)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchNodes(ctx, testNode(t), srv.URL+"/nodes.dat"); err == nil {
		t.Fatal("cancelled fetch succeeded")
	}
}

// A busy Kad port: Kad does not run, and the status says why.
func TestSetupKadBusyPort(t *testing.T) {
	busy, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	old := settings.KadPort
	settings.KadPort = uint16(busy.LocalAddr().(*net.UDPAddr).Port)
	defer func() { settings.KadPort = old }()
	var dc download.Config
	var m *download.Manager
	if k := setupKad(&dc, &m, nil, ed2k.Hash{1}); k != nil {
		t.Fatal("Kad set up on a busy port")
	}
	if dc.KadProblem == "" || dc.KadSearch != nil || dc.KadSources != nil || dc.Kad != nil {
		t.Fatalf("problem %q, hooks set", dc.KadProblem)
	}
	m = download.NewManager(dc)
	if m.KadProblem() == "" || m.KadStatus() != nil {
		t.Fatal("problem not reported")
	}
}
