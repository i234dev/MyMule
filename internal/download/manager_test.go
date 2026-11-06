package download

import (
	"bytes"
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/server"
)

func send(t *testing.T, c net.Conn, prot, op byte, payload []byte) {
	p := proto.Packet{Proto: prot, Opcode: op, Payload: payload}
	if _, err := c.Write(p.Encode()); err != nil {
		t.Logf("fake send: %v", err)
	}
}

// fakeServer answers login and source requests, pointing at peerAddr.
func fakeServer(t *testing.T, ln net.Listener, peerAddr *net.TCPAddr) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			for {
				p, err := proto.ReadPacket(c)
				if err != nil {
					return
				}
				switch p.Opcode {
				case proto.OpLoginRequest:
					var w proto.Writer
					w.U32(0x02000000) // HighID
					send(t, c, proto.ProtoEDonkey, proto.OpIDChange, w.B)
				case proto.OpGetSources:
					var w proto.Writer
					w.Bytes(p.Payload[:16])
					w.U8(1)
					w.U32(server.IPToU32(peerAddr.IP))
					w.U16(uint16(peerAddr.Port))
					send(t, c, proto.ProtoEDonkey, proto.OpFoundSources, w.B)
				}
			}
		}()
	}
}

// fakeUploader is a peer with the complete file. It queues us once and
// then accepts, serving data in 10 KB packets like eMule.
func fakeUploader(t *testing.T, ln net.Listener, data []byte, fh *ed2k.FileHashes, corruptOnce *atomic.Bool) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			for {
				p, err := proto.ReadPacket(c)
				if err != nil {
					return
				}
				r := proto.NewReader(p.Payload)
				switch {
				case p.Proto == proto.ProtoEDonkey && p.Opcode == peer.OpHello:
					h := peer.NewHello(ed2k.Hash{1}, "uploader", 0x02000000, 4662, nil, 0)
					send(t, c, proto.ProtoEDonkey, peer.OpHelloAnswer, h.Encode(true))
				case p.Opcode == peer.OpRequestFileName:
					var w proto.Writer
					w.Bytes(fh.Root[:])
					w.Str16("file.bin")
					send(t, c, proto.ProtoEDonkey, peer.OpReqFileNameAns, w.B)
				case p.Opcode == peer.OpSetReqFileID:
					var w proto.Writer
					w.Bytes(fh.Root[:])
					w.U16(0) // complete file
					send(t, c, proto.ProtoEDonkey, peer.OpFileStatus, w.B)
				case p.Opcode == peer.OpHashSetRequest:
					var w proto.Writer
					w.Bytes(fh.Root[:])
					w.U16(uint16(len(fh.Parts)))
					for _, h := range fh.Parts {
						w.Bytes(h[:])
					}
					send(t, c, proto.ProtoEDonkey, peer.OpHashSetAnswer, w.B)
				case p.Opcode == peer.OpStartUploadReq:
					var w proto.Writer
					w.U32(3)
					send(t, c, proto.ProtoEDonkey, peer.OpQueueRank, w.B)
					send(t, c, proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
				case p.Proto == proto.ProtoEDonkey && p.Opcode == peer.OpRequestParts:
					r.Hash()
					var st, en [3]uint32
					for i := range st {
						st[i] = r.U32()
					}
					for i := range en {
						en[i] = r.U32()
					}
					for i := range st {
						for off := st[i]; off < en[i]; off += 10240 {
							end := min(off+10240, en[i])
							chunk := append([]byte(nil), data[off:end]...)
							if corruptOnce.CompareAndSwap(true, false) {
								chunk[0] ^= 0xFF
							}
							var w proto.Writer
							w.Bytes(fh.Root[:])
							w.U32(off)
							w.U32(end)
							w.Bytes(chunk)
							send(t, c, proto.ProtoEDonkey, peer.OpSendingPart, w.B)
						}
					}
				}
			}
		}()
	}
}

func TestManagerEndToEnd(t *testing.T) {
	data := testData(ed2k.PartSize + 300000)
	fh, err := ed2k.HashReader(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatal(err)
	}

	peerLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer peerLn.Close()
	var corrupt atomic.Bool
	corrupt.Store(true) // first chunk is corrupted: part must be re-downloaded
	go fakeUploader(t, peerLn, data, fh, &corrupt)

	srvLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer srvLn.Close()
	go fakeServer(t, srvLn, peerLn.Addr().(*net.TCPAddr))

	dir := t.TempDir()
	pf, err := Create(dir, ed2k.Link{Name: "file.bin", Size: int64(len(data)), Hash: fh.Root})
	if err != nil {
		t.Fatal(err)
	}

	sa := srvLn.Addr().(*net.TCPAddr)
	var donePath string
	m := NewManager(Config{
		Nick:        "test",
		IncomingDir: dir + "/incoming",
		Connect: func(ctx context.Context, cfg server.Config) (*server.Conn, error) {
			cfg.Nick = "test"
			return server.Dial(ctx, server.Info{IP: sa.IP, Port: uint16(sa.Port)}, cfg)
		},
		Log:        t.Logf,
		OnComplete: func(_ ed2k.Hash, name, path string) { donePath = path },
		// The fake server hands out a source on 127.0.0.1.
		allowPrivateSources: true,
	})
	m.Add(pf)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(donePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded file differs")
	}
	_, files := m.Status()
	if len(files) != 1 || files[0].Corrupt != 1 {
		t.Fatalf("expected one corrupt part to be detected and fixed: %+v", files)
	}
}
