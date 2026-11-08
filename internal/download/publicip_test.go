package download

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"mymule/internal/proto"
	"mymule/internal/server"
)

// Our public IP comes from the HighID the server gives, unless that is a
// private or reserved address (a server must not make one "ours").
func TestPublicIPOnlyPublic(t *testing.T) {
	for _, tc := range []struct {
		ip   [4]byte
		want bool
	}{{[4]byte{5, 6, 7, 8}, true}, {[4]byte{10, 1, 2, 3}, false}, {[4]byte{127, 0, 0, 1}, false}} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			proto.ReadPacket(c)
			var w proto.Writer
			w.Bytes(tc.ip[:]) // the ID is the IP, in network order
			c.Write((&proto.Packet{Proto: proto.ProtoEDonkey, Opcode: proto.OpIDChange, Payload: w.B}).Encode())
			time.Sleep(500 * time.Millisecond)
		}()
		addr := ln.Addr().(*net.TCPAddr)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		srv, err := server.Dial(ctx, server.Info{IP: addr.IP, Port: uint16(addr.Port)}, server.Config{Nick: "test"})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		m := NewManager(Config{})
		m.srv = srv
		got := m.PublicIP()
		if got.IsValid() != tc.want || (tc.want && got != netip.AddrFrom4(tc.ip)) {
			t.Errorf("%v: public IP %v", tc.ip, got)
		}
		srv.Close()
		ln.Close()
	}
}
