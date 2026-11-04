package server

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// fakeUDPServer answers source requests and searches like a real server,
// concatenating two packets in one datagram as servers do.
func fakeUDPServer(t *testing.T) (*net.UDPConn, Info) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 2 || buf[0] != proto.ProtoEDonkey {
				continue
			}
			r := proto.NewReader(buf[2:n])
			var w proto.Writer
			switch buf[1] {
			case OpGlobGetSources2:
				for r.Len() >= 20 {
					h := r.Hash()
					if r.U32() == 0 {
						r.U64()
					}
					w.U8(proto.ProtoEDonkey)
					w.U8(OpGlobFoundSources)
					w.Bytes(h[:])
					w.U8(1)
					w.U32(IPToU32(net.IPv4(9, 9, 9, 9)))
					w.U16(4662)
				}
			case OpGlobSearchReq:
				for i := 0; i < 2; i++ {
					w.U8(proto.ProtoEDonkey)
					w.U8(OpGlobSearchRes)
					w.Bytes([]byte{byte(i), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
					w.U32(0)
					w.U16(0)
					w.U32(2)
					w.WriteTag(proto.StrTag(proto.FTFileName, "result.iso"))
					w.WriteTag(proto.IntTag(proto.FTSources, 5))
				}
			}
			c.WriteToUDP(w.B, from)
		}
	}()
	a := c.LocalAddr().(*net.UDPAddr)
	// The client sends to TCP port + 4.
	return c, Info{IP: a.IP, Port: uint16(a.Port - 4)}
}

func TestUDPSourcesAndSearch(t *testing.T) {
	srvConn, info := fakeUDPServer(t)
	defer srvConn.Close()

	u, err := ListenUDP()
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	var mu sync.Mutex
	got := map[ed2k.Hash][]Source{}
	u.OnSources = func(from *net.UDPAddr, h ed2k.Hash, srcs []Source) {
		mu.Lock()
		got[h] = srcs
		mu.Unlock()
	}
	go u.Run()

	files := []FileReq{{Hash: ed2k.Hash{1}, Size: 1000}, {Hash: ed2k.Hash{2}, Size: 5 << 30}}
	if err := u.GetSources(info, files); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(got) != 2 || got[ed2k.Hash{2}][0].Addr() != "9.9.9.9:4662" {
		t.Fatalf("sources: %v", got)
	}
	mu.Unlock()

	var results []Result
	err = u.Search(context.Background(), []Info{info}, Query{Keywords: "x"}, 500*time.Millisecond,
		func(from *net.UDPAddr, r Result) {
			if ServerKey(from) != info.Addr() {
				t.Errorf("server key %s != %s", ServerKey(from), info.Addr())
			}
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 2 || results[0].Name != "result.iso" || results[0].Sources != 5 {
		t.Fatalf("results: %+v", results)
	}
}
