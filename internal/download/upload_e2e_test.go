package download

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/server"
	"mymule/internal/share"
	"mymule/internal/upload"
)

func freePort(t *testing.T) uint16 {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func noServer(ctx context.Context, cfg server.Config) (*server.Conn, error) {
	return nil, errors.New("no server in this test")
}

// Two MyMule instances: A shares a file, B downloads it from A.
func TestUploadToAnotherMyMule(t *testing.T) {
	data := testData(ed2k.PartSize*2 + 123457)
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "shared.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	fh, _ := ed2k.HashReader(bytes.NewReader(data), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- A: the uploader ---
	lib := share.NewLibrary([]string{shared}, filepath.Join(t.TempDir(), "known.json"), t.Logf)
	go lib.Run(ctx, time.Hour)
	for lib.Get(fh.Root) == nil {
		if ctx.Err() != nil {
			t.Fatal("file never got shared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	portA := freePort(t)
	var a *Manager
	up := upload.New(upload.Config{
		Slots: 2, Library: lib, Debug: t.Logf,
		Dial: func(ip net.IP, port uint16) { a.DialForUpload(ip, port) },
	})
	a = NewManager(Config{
		UserHash: ed2k.Hash{0xA}, Nick: "uploader", Port: portA, ListenHost: "127.0.0.1", StayAlive: true,
		TempDir: t.TempDir(), IncomingDir: t.TempDir(),
		Connect: noServer, Library: lib, Upload: up, Debug: t.Logf,
	})
	go up.Run(ctx)
	go a.Run(ctx)

	// --- B: the downloader ---
	incoming := t.TempDir()
	var donePath string
	b := NewManager(Config{
		UserHash: ed2k.Hash{0xB}, Nick: "downloader", Port: freePort(t), ListenHost: "127.0.0.1",
		TempDir: t.TempDir(), IncomingDir: incoming, Connect: noServer, Debug: t.Logf,
		OnComplete: func(_ ed2k.Hash, name, path string) { donePath = path },
	})
	link := ed2k.Link{Name: "copy.bin", Size: int64(len(data)), Hash: fh.Root}
	if err := b.AddLink(link, "127.0.0.1:"+strconv.Itoa(int(portA))); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond) // let A start listening
	if err := b.Run(ctx); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(donePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded data differs from the shared file")
	}
	if st := up.Status(); st.Total < int64(len(data)) {
		t.Fatalf("uploader reports only %d bytes sent", st.Total)
	}
}

// A partial download only serves verified parts.
func TestPartialSharesOnlyVerifiedParts(t *testing.T) {
	data := testData(ed2k.PartSize + 1000)
	link, fh := linkFor(t, "p.bin", data)
	pf, err := Create(t.TempDir(), link)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	pf.SetHashSet(fh.Parts)
	// Download only the second (small) part.
	for _, b := range pf.Reserve("x", []bool{false, true}, 10) {
		pf.Write("x", b.Start, data[b.Start:b.End])
	}
	sp := sharedPart{pf}
	if av := sp.Avail(); av[0] || !av[1] {
		t.Fatalf("avail = %v", av)
	}
	buf := make([]byte, 100)
	if err := share.ReadFull(sp, buf, 0); err == nil {
		t.Fatal("served data from an unverified part")
	}
	if err := share.ReadFull(sp, buf, ed2k.PartSize); err != nil || !bytes.Equal(buf, data[ed2k.PartSize:ed2k.PartSize+100]) {
		t.Fatalf("verified part not served: %v", err)
	}
}
