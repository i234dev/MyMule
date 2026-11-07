package download

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// fakeKadPeer accepts one connection, answers the hello as a client with
// the given Kad version and returns the packets it gets after that.
func fakeKadPeer(t *testing.T, kadVersion byte) (uint16, chan *proto.Packet) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan *proto.Packet, 4)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		c := peer.NewConn(nc)
		defer c.Close()
		p, err := c.Read(5 * time.Second)
		if err != nil || p.Opcode != peer.OpHello {
			close(got)
			return
		}
		h := peer.NewHello(ed2k.Hash{9}, "checker", 0x05060708, 4662, nil, 0)
		if kadVersion > 0 {
			h = h.WithKad(4672, kadVersion)
		}
		c.Send(proto.ProtoEDonkey, peer.OpHelloAnswer, h.Encode(true))
		for {
			p, err := c.Read(3 * time.Second)
			if err != nil {
				close(got)
				return
			}
			got <- p
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port), got
}

func kadManager(t *testing.T) *Manager {
	m := NewManager(Config{UserHash: ed2k.Hash{3}, Nick: "me", Log: t.Logf, Debug: t.Logf,
		Kad: func() (uint16, byte) { return 4672, 8 }})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); m.wg.Wait() })
	m.ctx = ctx
	return m
}

// A node checking whether it is firewalled gets OP_KAD_FWTCPCHECK_ACK.
func TestKadConnectBack(t *testing.T) {
	m := kadManager(t)
	port, got := fakeKadPeer(t, 8)
	if !m.KadConnectBack(net.ParseIP("127.0.0.1"), port, ed2k.Hash{9}) {
		t.Fatal("refused")
	}
	select {
	case p, ok := <-got:
		if !ok || p.Proto != proto.ProtoEMule || p.Opcode != peer.OpKadFWTCPCheckAck {
			t.Fatalf("got %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no ack")
	}
	// Old Kad versions (before 7) do not understand it: nothing is sent
	// over TCP; they are told over UDP.
	var legacy atomic.Value
	m.cfg.KadLegacyAck = func(ip net.IP, port uint16) { legacy.Store(ip.String() + ":" + strconv.Itoa(int(port))) }
	port, got = fakeKadPeer(t, 6)
	m.KadConnectBack(net.ParseIP("127.0.0.1"), port, ed2k.Hash{})
	if p, ok := <-got; ok {
		t.Fatalf("sent %+v to an old client", p)
	}
	if legacy.Load() != "127.0.0.1:4672" {
		t.Fatalf("legacy ack: %v", legacy.Load())
	}
	// Another client answering at that address is not acked.
	port, got = fakeKadPeer(t, 8)
	m.KadConnectBack(net.ParseIP("127.0.0.1"), port, ed2k.Hash{7})
	if p, ok := <-got; ok {
		t.Fatalf("sent %+v to another client", p)
	}
}

func TestKadConnectBackLimits(t *testing.T) {
	m := kadManager(t)
	if m.KadConnectBack(net.ParseIP("127.0.0.1"), 0, ed2k.Hash{}) {
		t.Fatal("port 0 accepted")
	}
	m.connectBacks.Store(maxConnectBacks)
	if m.KadConnectBack(net.ParseIP("127.0.0.1"), 1, ed2k.Hash{}) {
		t.Fatal("more than the limit at once")
	}
	m.connectBacks.Store(0)
	m.port = 4662
	if m.KadConnectBack(net.ParseIP("127.0.0.1"), 4662, m.cfg.UserHash) {
		t.Fatal("asked to connect to ourselves")
	}
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	if m.KadConnectBack(net.ParseIP("127.0.0.1"), 1, ed2k.Hash{}) {
		t.Fatal("accepted while shutting down")
	}
	stopped := NewManager(Config{})
	if stopped.KadConnectBack(net.ParseIP("127.0.0.1"), 1, ed2k.Hash{}) {
		t.Fatal("accepted before Run")
	}
}

// Our hello tells peers our Kad port and version; an incoming ack reaches
// the Kad side with the peer's address.
func TestKadHelloAndAck(t *testing.T) {
	m := kadManager(t)
	h := m.ourHello()
	if h.KadPort() != 4672 || h.KadVersion() != 8 {
		t.Fatalf("hello: port %d version %d", h.KadPort(), h.KadVersion())
	}
	var acked atomic.Value
	m.cfg.KadFirewallAck = func(ip net.IP) { acked.Store(ip.String()) }
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			defer nc.Close()
			time.Sleep(time.Second)
		}
	}()
	nc, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c := peer.NewConn(nc)
	defer c.Close()
	if !m.kadPacket(c, &proto.Packet{Proto: proto.ProtoEMule, Opcode: peer.OpKadFWTCPCheckAck}) {
		t.Fatal("ack not handled")
	}
	if acked.Load() != "127.0.0.1" {
		t.Fatalf("acked %v", acked.Load())
	}
	if m.kadPacket(c, &proto.Packet{Proto: proto.ProtoEDonkey, Opcode: peer.OpKadFWTCPCheckAck}) {
		t.Fatal("eDonkey packet taken as Kad")
	}
	_ = strconv.Itoa
}
