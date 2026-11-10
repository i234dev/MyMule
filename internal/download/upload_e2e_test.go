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

	credits "mymule/internal/credit"
	"mymule/internal/ed2k"
	"mymule/internal/obfus"
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
func TestUploadToAnotherMyMule(t *testing.T) { uploadBetween(t, 0, false) }

// The same with obfuscation required on both sides (so the connection
// can only be obfuscated) and credits: each identifies the other, and
// counts what went from A to B.
func TestUploadObfuscatedWithCredits(t *testing.T) {
	uploadBetween(t, obfus.Supports|obfus.Requests|obfus.Requires, true)
}

func uploadBetween(t *testing.T, obf byte, withCredits bool) {
	var credA, credB *credits.Store
	if withCredits {
		var err error
		if credA, err = credits.Open(t.TempDir(), t.Logf); err != nil {
			t.Fatal(err)
		}
		if credB, err = credits.Open(t.TempDir(), t.Logf); err != nil {
			t.Fatal(err)
		}
	}
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
		Dial: func(ip net.IP, port uint16, h ed2k.Hash, c byte) { a.DialForUpload(ip, port, h, c) },
	})
	if credA != nil {
		up = upload.New(upload.Config{
			Slots: 2, Library: lib, Debug: t.Logf, Ratio: credA.Ratio, Uploaded: credA.Uploaded,
			Dial: func(ip net.IP, port uint16, h ed2k.Hash, c byte) { a.DialForUpload(ip, port, h, c) },
		})
	}
	a = NewManager(Config{
		UserHash: ed2k.Hash{0xA}, Nick: "uploader", Port: portA, ListenHost: "127.0.0.1", StayAlive: true,
		TempDir: t.TempDir(), IncomingDir: t.TempDir(),
		Connect: noServer, Library: lib, Upload: up, Debug: t.Logf,
		Credits: credA, Obfuscation: obf,
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
		Credits:    credB, Obfuscation: obf,
	})
	b.cfg.allowPrivateSources = true
	link := ed2k.Link{Name: "copy.bin", Size: int64(len(data)), Hash: fh.Root}
	if err := b.AddLink(link, "127.0.0.1:"+strconv.Itoa(int(portA))); err != nil {
		t.Fatal(err)
	}
	if obf != 0 {
		// As a server or Kad would give it: A's user hash and crypt options.
		b.mu.Lock()
		for _, src := range b.files[fh.Root].sources {
			src.UserHash, src.Crypt = ed2k.Hash{0xA}, obfus.Supports
		}
		b.mu.Unlock()
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
	} else if st.Saved < int64(len(data))/2 {
		// The data compresses well and both sides support compression.
		t.Fatalf("compression saved only %d bytes", st.Saved)
	}
	if withCredits {
		ip := net.IPv4(127, 0, 0, 1)
		ca, _ := credA.Get(ed2k.Hash{0xB}, ip)
		cb, _ := credB.Get(ed2k.Hash{0xA}, ip)
		if !ca.Identified || !cb.Identified || ca.Uploaded < uint64(len(data)) || cb.Downloaded < uint64(len(data)) {
			t.Fatalf("credits: A has %+v for B, B has %+v for A", ca, cb)
		}
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
