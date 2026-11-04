package server

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"mymule/internal/proto"
)

func TestServerMetRoundTrip(t *testing.T) {
	in := []Info{
		{IP: net.IPv4(1, 2, 3, 4).To4(), Port: 4661, Name: "Test One", Users: 100},
		{IP: net.IPv4(5, 6, 7, 8).To4(), Port: 4242, Name: "Test Two", Description: "desc"},
	}
	var buf bytes.Buffer
	if err := WriteServerMet(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ParseServerMet(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Addr() != "1.2.3.4:4661" || out[0].Name != "Test One" ||
		out[0].Users != 100 || out[1].Description != "desc" {
		t.Fatalf("got %+v", out)
	}
}

func TestQueryEncode(t *testing.T) {
	b, err := Query{Keywords: "foo bar"}.encode()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x00, 0x01, 3, 0, 'f', 'o', 'o', 0x01, 3, 0, 'b', 'a', 'r'}
	if !bytes.Equal(b, want) {
		t.Fatalf("got % x", b)
	}
}

// fakeServer accepts one connection, checks the login and answers a search.
func fakeServer(t *testing.T, ln net.Listener) {
	c, err := ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()

	p, err := proto.ReadPacket(c)
	if err != nil || p.Opcode != proto.OpLoginRequest {
		t.Errorf("expected login, got %+v %v", p, err)
		return
	}
	var w proto.Writer
	w.Str16("welcome")
	c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpServerMessage, Payload: w.B}).Encode())
	w = proto.Writer{}
	w.U32(0x02000000) // HighID
	c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpIDChange, Payload: w.B}).Encode())

	p, err = proto.ReadPacket(c)
	if err != nil || p.Opcode != proto.OpSearchRequest {
		t.Errorf("expected search, got %+v %v", p, err)
		return
	}
	w = proto.Writer{}
	w.U32(1)
	w.Bytes(bytes.Repeat([]byte{0xAB}, 16))
	w.U32(0)
	w.U16(0)
	w.U32(3)
	w.WriteTag(proto.StrTag(proto.FTFileName, "ubuntu.iso"))
	w.WriteTag(proto.IntTag(proto.FTFileSize, 123456))
	w.WriteTag(proto.IntTag(proto.FTSources, 7))
	c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpSearchResult, Payload: w.B}).Encode())
	time.Sleep(100 * time.Millisecond)
}

func TestDialAndSearch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go fakeServer(t, ln)

	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Info{IP: addr.IP, Port: uint16(addr.Port)}, Config{Nick: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.Status().HighID {
		t.Fatalf("expected HighID, got %+v", c.Status())
	}
	res, _, err := c.Search(ctx, Query{Keywords: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Name != "ubuntu.iso" || res[0].Size != 123456 || res[0].Sources != 7 {
		t.Fatalf("results %+v", res)
	}
}
