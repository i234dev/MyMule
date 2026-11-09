package download

import (
	"net"
	"testing"
	"time"

	credits "mymule/internal/credit"
	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// identifyOver makes peer (hash ph, at ip) prove its identity to our
// store, with its own store, as over a connection.
func identifyOver(t *testing.T, ours, theirs *credits.Store, ph ed2k.Hash, ip net.IP) {
	t.Helper()
	opts := proto.TagList{proto.IntTag(peer.CTEMuleMiscOpts1, 3<<16)}
	var toUs, toThem []*proto.Packet
	mk := func(q *[]*proto.Packet) func(byte, []byte) error {
		return func(op byte, p []byte) error {
			*q = append(*q, &proto.Packet{Proto: proto.ProtoEMule, Opcode: op, Payload: p})
			return nil
		}
	}
	usIP := net.IPv4(127, 0, 0, 2)
	our := ours.NewIdent(peer.Hello{UserHash: ph, Tags: opts}, ip, func() net.IP { return usIP }, mk(&toThem))
	their := theirs.NewIdent(peer.Hello{UserHash: ed2k.Hash{0x0E}, Tags: opts}, usIP, func() net.IP { return ip }, mk(&toUs))
	our.Start()
	their.Start()
	for range 10 {
		a, b := toThem, toUs
		toThem, toUs = nil, nil
		for _, p := range a {
			their.Handle(p)
		}
		for _, p := range b {
			our.Handle(p)
		}
	}
	if !ours.Identified(ph, ip) {
		t.Fatal("peer not identified")
	}
}

// An identified peer that sends the same bytes again earns no credit
// for them: credits count what is new to us.
func TestCreditsOnlyForNewBytes(t *testing.T) {
	ours, err := credits.Open(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := credits.Open(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ph := ed2k.Hash{0x0F}
	ip := net.IPv4(127, 0, 0, 1)
	identifyOver(t, ours, theirs, ph, ip)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		pc := peer.NewConn(c)
		for {
			if _, err := pc.Read(5 * time.Second); err != nil {
				return
			}
		}
	}()
	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	pf, data, _ := blameSetup(t)
	m := NewManager(Config{Credits: ours})
	f := &File{pf: pf, sources: map[string]*Source{}}
	m.files = map[ed2k.Hash]*File{pf.Hash: f}
	src := &Source{key: "127.0.0.1:4662", file: f}
	s := &session{m: m, c: peer.NewConn(nc), src: src, f: f, pf: pf, owner: src.key,
		hello: peer.Hello{UserHash: ph}}
	held := pf.Reserve(s.owner, nil, 3)
	s.pending = held
	b := held[0]
	chunk := data[b.Start : b.Start+1024]
	for range 20 {
		if err := s.store(b.Start, chunk); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := ours.Get(ph, ip)
	if got.Downloaded != uint64(len(chunk)) {
		t.Fatalf("credited %d bytes for %d new ones sent 20 times", got.Downloaded, len(chunk))
	}
	// The all-time count is of what was received, resent bytes too.
	if m.Received() != int64(20*len(chunk)) {
		t.Fatalf("received %d", m.Received())
	}
}
