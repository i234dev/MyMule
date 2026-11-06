package server

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"mymule/internal/proto"
)

// A server that stays silent after login must not lose the connection:
// MyMule checks it with keep-alives (empty file offers) instead.
func TestSilentServerKeptAlive(t *testing.T) {
	old := readIdle
	readIdle = 150 * time.Millisecond
	defer func() { readIdle = old }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var keepAlives atomic.Int32
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if p, err := proto.ReadPacket(c); err != nil || p.Opcode != proto.OpLoginRequest {
			return
		}
		var w proto.Writer
		w.U32(0x02000000)
		c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpIDChange, Payload: w.B}).Encode())
		for { // then silence: only read what MyMule sends
			p, err := proto.ReadPacket(c)
			if err != nil {
				return
			}
			if p.Opcode == proto.OpOfferFiles && len(p.Payload) == 4 {
				keepAlives.Add(1)
			}
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Info{IP: addr.IP, Port: uint16(addr.Port)}, Config{Nick: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(time.Second)
	select {
	case <-c.Done():
		t.Fatalf("connection to a silent server dropped: %v", c.Err())
	default:
	}
	if keepAlives.Load() < 2 {
		t.Fatalf("%d keep-alives sent in a second of silence, want several", keepAlives.Load())
	}
}
