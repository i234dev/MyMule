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
// another port or with another LowID: its address is banned too.
func TestBanCoversOtherPortsAndIDs(t *testing.T) {
	pf, _, _ := blameSetup(t)
	m := NewManager(Config{TempDir: t.TempDir(), IncomingDir: t.TempDir()})
	user := ed2k.Hash{0xba, 0xd0}
	pf.BanIdentities("203.0.113.5:4662", "ip:203.0.113.5")

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	src := &Source{}
	s := &session{m: m, c: peer.NewConn(a), src: src, pf: pf, owner: "203.0.113.5:4999",
		hello: peer.NewHello(user, "back again", 0x02000000, 4999, nil, 0)}
	if err := s.run(context.Background()); !errors.Is(err, errBanned) || src.State != StateNoFile {
		t.Fatalf("banned peer on another port: %v, state %v", err, src.State)
	}
	if !pf.BannedAny("ip:203.0.113.5") || pf.BannedAny("ip:203.0.113.6") {
		t.Fatal("BannedAny")
	}
}

// The user hash a peer gives is not proof of who it is: a peer banned for
// corrupt data does not get the honest source whose hash it gave banned.
func TestBanNotByClaimedUserHash(t *testing.T) {
	pf, _, _ := blameSetup(t)
	m := NewManager(Config{TempDir: t.TempDir(), IncomingDir: t.TempDir()})
	honest := ed2k.Hash{0x60, 0x0d}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	liar := &session{m: m, c: peer.NewConn(a), src: &Source{}, pf: pf, owner: "203.0.113.9:4662",
		hello: peer.NewHello(honest, "liar", 0x02000000, 4662, nil, 0)}
	pf.BanIdentities(liar.identities()...)
	for _, id := range liar.identities() {
		if id == "user:"+honest.String() {
			t.Fatal("ban covers the claimed user hash")
		}
	}
	if pf.BannedAny("198.51.100.4:4662", "ip:198.51.100.4", "user:"+honest.String()) {
		t.Fatal("the honest source is banned")
	}
}
