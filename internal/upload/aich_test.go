package upload

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mymule/internal/aich"
	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/share"
)

// A complete shared file's AICH hash is given (its tree computed in the
// background the first time it is asked for), and so is the recovery
// data of a part asked with the right hash; with another hash, or over
// budget, an empty answer.
func TestServeAICH(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 2*aich.PartSize+1000)
	for i := range data {
		data[i] = byte(i*7 + i>>13)
	}
	p := filepath.Join(dir, "file.bin")
	os.WriteFile(p, data, 0o644)
	fh, _ := ed2k.HashReader(bytes.NewReader(data), nil)
	tree, _ := aich.FromReader(bytes.NewReader(data), uint64(len(data)))
	lib := share.NewLibrary([]string{dir}, filepath.Join(t.TempDir(), "known.json"), nil)
	lib.AddComplete(p, fh.Root, fh.Parts) // known, without its AICH tree
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lib.Run(ctx, time.Hour)
	m := New(Config{Slots: 1, Library: lib})

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	us, them := peer.NewConn(a), peer.NewConn(b)
	ask := func(op byte, payload []byte) *proto.Packet {
		t.Helper()
		go m.Handle(us, peer.Hello{}, net.ParseIP("198.51.100.1"), &proto.Packet{Proto: proto.ProtoEMule, Opcode: op, Payload: payload})
		pk, err := them.Read(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return pk
	}

	// Not known at first: no answer; then computed.
	if root, ok := m.aichRoot(fh.Root); ok {
		t.Fatalf("root %s before it was computed", root)
	}
	for end := time.Now().Add(10 * time.Second); lib.AICH(fh.Root) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("AICH tree never computed")
		}
	}
	pk := ask(peer.OpAICHFileHashReq, fh.Root[:])
	if f, root, err := peer.DecodeAICHFileHash(pk.Payload); pk.Opcode != peer.OpAICHFileHashAns || err != nil || f != fh.Root || root != tree.Root() {
		t.Fatalf("file hash answer: %v %s", err, root)
	}

	pk = ask(peer.OpAICHRequest, peer.EncodeAICHRequest(fh.Root, 1, tree.Root()))
	f, part, root, rec, err := peer.DecodeAICHAnswer(pk.Payload)
	if pk.Opcode != peer.OpAICHAnswer || err != nil || f != fh.Root || part != 1 || root != tree.Root() || rec == nil {
		t.Fatalf("answer: %v", err)
	}
	blocks, err := aich.Verify(uint64(len(data)), 1, tree.Root(), rec)
	if err != nil || blocks[3] != aich.Block(data[aich.PartSize+3*aich.BlockSize:aich.PartSize+4*aich.BlockSize]) {
		t.Fatalf("recovery data: %v", err)
	}

	// Another root, or a part without recovery data: empty answers.
	for _, req := range [][]byte{
		peer.EncodeAICHRequest(fh.Root, 1, aich.Hash{1}),
		peer.EncodeAICHRequest(fh.Root, 2, tree.Root()), // 1000 bytes: no recovery
		peer.EncodeAICHRequest(ed2k.Hash{1}, 0, tree.Root()),
	} {
		pk = ask(peer.OpAICHRequest, req)
		if _, _, _, rec, err := peer.DecodeAICHAnswer(pk.Payload); err != nil || rec != nil || len(pk.Payload) != 16 {
			t.Fatalf("empty answer: %v %d", err, len(pk.Payload))
		}
	}
	// Over budget: empty.
	m.recovery.mu.Lock()
	m.recovery.n = maxRecoveryPerMinute
	m.recovery.mu.Unlock()
	pk = ask(peer.OpAICHRequest, peer.EncodeAICHRequest(fh.Root, 0, tree.Root()))
	if len(pk.Payload) != 16 {
		t.Fatal("recovery data over budget")
	}
}
