package download

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
	"mymule/internal/server"
)

// A server that repeats a result cannot add its sources again, and none
// can claim an absurd number; different servers still add up.
func TestResultMergerCannotBeInflated(t *testing.T) {
	var mg resultMerger
	fake := server.Result{Hash: ed2k.Hash{1}, Name: "fake.mkv", Size: 1000, Sources: 900, CompleteSources: 900}
	for i := 0; i < 1000; i++ {
		mg.add("203.0.113.1", fake)
	}
	mg.add("203.0.113.1", server.Result{Hash: ed2k.Hash{2}, Name: "huge.mkv", Size: 1000, Sources: 4_000_000_000, CompleteSources: 4_000_000_000})
	real := server.Result{Hash: ed2k.Hash{3}, Name: "real.mkv", Size: 1000, Sources: 40, CompleteSources: 30}
	mg.add("198.51.100.1", real)
	mg.add("198.51.100.2", real)

	res, servers := mg.results()
	if servers != 3 {
		t.Fatalf("%d servers, want 3", servers)
	}
	got := map[string]server.Result{}
	for _, r := range res {
		got[r.Name] = r
	}
	if got["fake.mkv"].Sources != 900 {
		t.Errorf("repeated result: %d sources, want 900", got["fake.mkv"].Sources)
	}
	if got["huge.mkv"].Sources != maxReportedSources || got["huge.mkv"].CompleteSources != maxReportedSources {
		t.Errorf("absurd claim: %d/%d sources, want the cap %d", got["huge.mkv"].Sources, got["huge.mkv"].CompleteSources, maxReportedSources)
	}
	if got["real.mkv"].Sources != 80 || got["real.mkv"].CompleteSources != 60 {
		t.Errorf("two servers: %d/%d sources, want 80/60", got["real.mkv"].Sources, got["real.mkv"].CompleteSources)
	}
}

func TestResultMergerLimits(t *testing.T) {
	var mg resultMerger
	for i := 0; i < maxGlobalResults+500; i++ {
		mg.add("203.0.113.1", server.Result{Hash: ed2k.Hash{byte(i), byte(i >> 8), 1}, Size: 1, Sources: 1})
	}
	if res, _ := mg.results(); len(res) != maxGlobalResults {
		t.Fatalf("%d results kept, want %d", len(res), maxGlobalResults)
	}
	if satAdd(^uint32(0)-1, 5) != ^uint32(0) || satAdd(2, 3) != 5 {
		t.Fatal("satAdd wraps or miscounts")
	}
}

// A download that completed but was never moved (the final step failed,
// or MyMule stopped first) is finished by the manager on its own.
func TestManagerFinishesCompleteDownloads(t *testing.T) {
	dir := t.TempDir()
	data := testData(4000)
	link, _ := linkFor(t, "left.bin", data)
	pf, err := Create(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	download(t, pf, data) // complete, but nobody called Finish
	done := make(chan string, 1)
	m := NewManager(Config{IncomingDir: filepath.Join(dir, "incoming"),
		OnComplete: func(_ ed2k.Hash, _, path string) { done <- path }})
	m.Add(pf)
	m.retryFinish()
	select {
	case p := <-done:
		if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
			t.Fatal("finished file differs")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("complete download was not finished")
	}
}

// fakeUDPSearchServer answers UDP keyword searches with one result.
func fakeUDPSearchServer(t *testing.T) server.Info {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 2 || buf[0] != proto.ProtoEDonkey || buf[1] != server.OpGlobSearchReq {
				continue
			}
			var w proto.Writer
			w.U8(proto.ProtoEDonkey)
			w.U8(server.OpGlobSearchRes)
			w.Bytes([]byte{9, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
			w.U32(0)
			w.U16(0)
			w.U32(3)
			w.WriteTag(proto.StrTag(proto.FTFileName, "udp-result.iso"))
			w.WriteTag(proto.IntTag(proto.FTFileSize, 734003200))
			w.WriteTag(proto.IntTag(proto.FTSources, 5))
			c.WriteToUDP(w.B, from)
		}
	}()
	a := c.LocalAddr().(*net.UDPAddr)
	return server.Info{IP: a.IP, Port: uint16(a.Port - 4)} // UDP port = TCP port + 4
}

// The connected server accepts the login but never answers searches (as
// a busy server may). The other servers must still be asked, and the
// search must come back with their results quickly instead of spending
// all its time waiting for the connected one.
func TestGlobalSearchDoesNotWaitForSilentServer(t *testing.T) {
	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srvLn.Close()
	go fakeServer(t, srvLn, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}) // ignores searches
	sa := srvLn.Addr().(*net.TCPAddr)
	other := fakeUDPSearchServer(t)

	m := NewManager(Config{Servers: func() []server.Info { return []server.Info{other} }})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := server.Dial(ctx, server.Info{IP: sa.IP, Port: uint16(sa.Port)}, server.Config{Nick: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	u, err := server.ListenUDP("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	go u.Run()
	m.mu.Lock()
	m.srv, m.udp = conn, u
	m.mu.Unlock()

	start := time.Now()
	sctx, scancel := context.WithTimeout(ctx, 20*time.Second) // the old code spent all of it
	defer scancel()
	res, _, err := m.SearchGlobal(sctx, server.Query{Keywords: "example"}, 500*time.Millisecond)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Name != "udp-result.iso" {
		t.Fatalf("results %+v, want the UDP server's one", res)
	}
	if took > 500*time.Millisecond+connectedGrace+2*time.Second {
		t.Fatalf("search took %v waiting for a server that never answers", took)
	}
}
