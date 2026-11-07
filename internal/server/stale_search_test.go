package server

import (
	"context"
	"net"
	"testing"
	"time"

	"mymule/internal/proto"
)

// A search that timed out may still be answered later. That late answer
// is thrown away, not returned as the answer to the next search.
func TestLateSearchAnswerNotReused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	answer := func(name string) []byte {
		var w proto.Writer
		w.U32(1)
		w.Bytes(make([]byte, 16))
		w.U32(0)
		w.U16(0)
		w.U32(2)
		w.WriteTag(proto.StrTag(proto.FTFileName, name))
		w.WriteTag(proto.IntTag(proto.FTFileSize, 1000))
		return (&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpSearchResult, Payload: w.B}).Encode()
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		proto.ReadPacket(c) // login
		var w proto.Writer
		w.U32(0x02000000)
		c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpIDChange, Payload: w.B}).Encode())
		proto.ReadPacket(c)           // first search: not answered in time
		proto.ReadPacket(c)           // second search
		c.Write(answer("first.iso"))  // the late answer to the first
		c.Write(answer("second.iso")) // then the second
		time.Sleep(500 * time.Millisecond)
	}()

	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Info{IP: addr.IP, Port: uint16(addr.Port)}, Config{Nick: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	short, cancelShort := context.WithTimeout(ctx, 100*time.Millisecond)
	if _, _, err := c.Search(short, Query{Keywords: "first"}); err == nil {
		t.Fatal("first search did not time out")
	}
	cancelShort()
	res, _, err := c.Search(ctx, Query{Keywords: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Name != "second.iso" {
		t.Fatalf("second search got %+v", res)
	}
}
