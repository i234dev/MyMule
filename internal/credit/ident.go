package credit

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

// What a peer asks for in OP_SECIDENTSTATE.
const (
	needNothing   = 0
	needSignature = 1
	needKeyAndSig = 2
)

// IP kinds of a version 2 signature: which address was signed with it.
const (
	ipRemote = 10 // the receiver's address, as the signer sees it
	ipLocal  = 20 // the signer's own address
	ipNone   = 30
)

// maxSignatures is how many signatures we make on one connection: one is
// normal, and each costs some CPU.
const maxSignatures = 3

// Ident runs the secure identification on one peer connection, both
// ways: the peer proves to us who it is, and we to it. Its methods are
// called from the connection's reading goroutine only.
type Ident struct {
	s       *Store
	hash    ed2k.Hash
	ip      net.IP
	support byte
	send    func(op byte, payload []byte) error
	// ourIP returns our public IPv4 address when we know it (HighID),
	// for version 2 signatures; nil when we do not.
	ourIP func() net.IP

	challenge uint32 // ours, for the peer to sign; 0 = none sent
	verified  bool   // a signature was checked (one per connection)
	key       []byte // the peer's key: stored or received on this connection

	theirNeed      byte   // what the peer asked us for
	theirChallenge []byte // the 4 bytes we sign
	signed         int
}

// NewIdent prepares the identification with a peer that sent hello h
// from ip; send sends an eMule-protocol packet on the connection.
func (s *Store) NewIdent(h peer.Hello, ip net.IP, ourIP func() net.IP, send func(op byte, payload []byte) error) *Ident {
	if ourIP == nil {
		ourIP = func() net.IP { return nil }
	}
	return &Ident{s: s, hash: h.UserHash, ip: ip.To4(), support: h.SecIdent(), send: send, ourIP: ourIP, key: s.KeyOf(h.UserHash)}
}

// Start asks the peer to prove its identity, if it supports it and has
// not done so from this address yet.
func (id *Ident) Start() error {
	if id.support&3 == 0 || id.ip == nil || id.s.Identified(id.hash, id.ip) {
		return nil
	}
	need := byte(needSignature)
	if id.key == nil {
		need = needKeyAndSig
	}
	var b [4]byte
	for id.challenge == 0 {
		rand.Read(b[:])
		id.challenge = binary.LittleEndian.Uint32(b[:])
	}
	return id.send(peer.OpSecIdentState, append([]byte{need}, b[:]...))
}

// Handle processes p if it belongs to the identification; it reports
// whether it did.
func (id *Ident) Handle(p *proto.Packet) (bool, error) {
	if p.Proto != proto.ProtoEMule {
		return false, nil
	}
	switch p.Opcode {
	case peer.OpSecIdentState:
		return true, id.onState(p.Payload)
	case peer.OpPublicKey:
		return true, id.onKey(p.Payload)
	case peer.OpSignature:
		id.onSignature(p.Payload)
		return true, nil
	}
	return false, nil
}

func (id *Ident) onState(b []byte) error {
	if len(b) != 5 || id.support&3 == 0 {
		return nil
	}
	id.theirNeed = b[0]
	id.theirChallenge = append([]byte(nil), b[1:]...)
	switch id.theirNeed {
	case needKeyAndSig:
		pub := id.s.PublicKey()
		if err := id.send(peer.OpPublicKey, append([]byte{byte(len(pub))}, pub...)); err != nil {
			return err
		}
		fallthrough
	case needSignature:
		return id.sign()
	}
	return nil
}

func (id *Ident) onKey(b []byte) error {
	if len(b) < 2 || int(b[0]) != len(b)-1 || id.key != nil || id.support&3 == 0 {
		return nil // a key we already have cannot be replaced
	}
	if _, err := parsePublicKey(b[1:]); err != nil {
		return nil
	}
	id.key = append([]byte(nil), b[1:]...)
	// We could not sign before: our signature covers the peer's key.
	if id.theirNeed == needSignature || id.theirNeed == needKeyAndSig {
		return id.sign()
	}
	return nil
}

// sign sends our signature of the peer's key and its challenge.
func (id *Ident) sign() error {
	if id.key == nil || id.theirChallenge == nil || id.signed >= maxSignatures {
		return nil
	}
	msg := append(append([]byte(nil), id.key...), id.theirChallenge...)
	// eMule signs with version 1 when the peer supports it.
	v2 := id.support&1 == 0
	var kind byte
	if v2 {
		kind = ipRemote
		ip := id.ip
		if our := id.ourIP(); our.To4() != nil {
			kind, ip = ipLocal, our.To4()
		}
		msg = append(append(msg, ip...), kind)
	}
	sig := id.s.key.sign(msg)
	if sig == nil {
		return nil
	}
	id.signed++
	id.theirNeed = needNothing
	out := append([]byte{byte(len(sig))}, sig...)
	if v2 {
		out = append(out, kind)
	}
	return id.send(peer.OpSignature, out)
}

func (id *Ident) onSignature(b []byte) {
	if len(b) < 2 || id.verified || id.challenge == 0 || id.key == nil {
		return
	}
	var kind byte
	switch {
	case int(b[0]) == len(b)-1:
	case int(b[0]) == len(b)-2 && id.support&2 != 0:
		kind = b[len(b)-1]
	default:
		return
	}
	id.verified = true // one try per connection: each check costs CPU
	k, err := parsePublicKey(id.key)
	if err != nil {
		return
	}
	msg := binary.LittleEndian.AppendUint32(append([]byte(nil), id.s.PublicKey()...), id.challenge)
	if kind != 0 {
		var ip net.IP
		switch kind {
		case ipLocal:
			ip = id.ip
		case ipRemote:
			ip = id.ourIP().To4()
		case ipNone:
			ip = net.IPv4zero.To4()
		}
		if ip == nil {
			return
		}
		msg = append(append(msg, ip...), kind)
	}
	// A key received on this connection becomes the peer's for good; a
	// stored one must be the one used.
	if !k.verify(msg, b[1:1+int(b[0])]) {
		id.s.Debug("credits: %s (%s) failed to prove its identity", id.hash, id.ip)
		return
	}
	if stored := id.s.KeyOf(id.hash); stored != nil && !bytes.Equal(stored, id.key) {
		return
	}
	id.s.verified(id.hash, id.ip, id.key)
	id.s.Debug("credits: %s (%s) proved its identity", id.hash, id.ip)
}

// Identified reports whether the peer proved its identity.
func (id *Ident) Identified() bool { return id.s.Identified(id.hash, id.ip) }
