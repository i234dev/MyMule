package download

import (
	"context"
	"errors"
	"net"
	"testing"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
)

// A peer banned for corrupt data stays banned when it comes back from
// another port or with another LowID: its user hash (and address) are
// banned too.
func TestBanCoversOtherPortsAndIDs(t *testing.T) {
	pf, _, _ := blameSetup(t)
	m := NewManager(Config{TempDir: t.TempDir(), IncomingDir: t.TempDir()})
	user := ed2k.Hash{0xba, 0xd0}
	pf.BanIdentities("203.0.113.5:4662", "ip:203.0.113.5", "user:"+user.String())

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	src := &Source{}
	s := &session{m: m, c: peer.NewConn(a), src: src, pf: pf, owner: "203.0.113.5:4999",
		hello: peer.NewHello(user, "back again", 0x02000000, 4999, nil, 0)}
	if err := s.run(context.Background()); !errors.Is(err, errBanned) || src.State != StateNoFile {
		t.Fatalf("banned peer on another port: %v, state %v", err, src.State)
	}
	if !pf.BannedAny("ip:203.0.113.5") || pf.BannedAny("ip:203.0.113.6", "user:"+(ed2k.Hash{1}).String()) {
		t.Fatal("BannedAny")
	}
}
