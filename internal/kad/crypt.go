package kad

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"net/netip"
)

// Obfuscated datagrams (eMule's EncryptedDatagramSocket). A datagram is
// RC4-encrypted with the MD5 of a key both sides know plus two random
// bytes sent in clear:
//
//	[marker][u16 random][magic][pad len][pad][Kad only: u32 receiver
//	verify key][u32 sender verify key][packet]
//
// everything from the magic on being encrypted. The marker is a random
// byte that is never one of the plain protocol bytes, and its two low bits
// say which key was used: 00 the receiver's Kad ID, 10 a verify key the
// receiver gave us, x1 the receiver's eD2K user hash (client UDP, not Kad).
//
// Verify keys stop forged source addresses: each node gives every other
// node a key derived from a secret and that node's IP. A datagram that
// carries it back must come from that IP, since only that IP received it.
const (
	cryptHeaderLen  = 8
	magicSyncClient = 0x395F2EC1
	magicUDP        = 91
)

// Keys are what our node needs to read and write obfuscated datagrams.
type Keys struct {
	KadID    ID       // our Kad ID
	UserHash [16]byte // our eD2K user hash
	UDPKey   uint32   // secret behind our verify keys; kept across restarts
}

// as4 returns ip's 4 bytes, and false when it is not an IPv4 address
// (Kad is IPv4 only).
func as4(ip netip.Addr) ([4]byte, bool) {
	ip = ip.Unmap()
	if !ip.Is4() {
		return [4]byte{}, false
	}
	return ip.As4(), true
}

// VerifyKey is the key we give the node at ip: it sends it back in
// obfuscated datagrams to show they come from that IP. Never 0; 0 for an
// address that is not IPv4.
func (k *Keys) VerifyKey(ip netip.Addr) uint32 {
	var buf [8]byte
	a, ok := as4(ip)
	if !ok {
		return 0
	}
	copy(buf[:4], a[:])
	binary.LittleEndian.PutUint32(buf[4:], k.UDPKey)
	sum := md5.Sum(buf[:])
	v := binary.LittleEndian.Uint32(sum[0:]) ^ binary.LittleEndian.Uint32(sum[4:]) ^
		binary.LittleEndian.Uint32(sum[8:]) ^ binary.LittleEndian.Uint32(sum[12:])
	return v%0xFFFFFFFE + 1
}

// Received is a datagram read from the Kad port.
type Received struct {
	Data      []byte // the plain datagram: [protocol][opcode][payload]
	Encrypted bool
	// For obfuscated Kad datagrams: the verify key the sender gave back to
	// us (ValidKey says whether it is the one we gave its IP, so the
	// address is not forged) and the one it gives us for our next packets.
	ValidKey  bool
	SenderKey uint32
}

// ErrBadCrypt is returned for a datagram that is neither plain nor
// obfuscated with a key we know.
var ErrBadCrypt = errors.New("kad: unreadable datagram")

func rc4For(parts ...[]byte) *rc4.Cipher {
	h := md5.New()
	for _, p := range parts {
		h.Write(p)
	}
	c, _ := rc4.NewCipher(h.Sum(nil)) // a 16-byte key is always valid
	return c
}

// Decrypt reads a datagram received from ip. Plain datagrams are returned
// as they are.
func (k *Keys) Decrypt(b []byte, ip netip.Addr) (Received, error) {
	if len(b) <= cryptHeaderLen || isPlainMarker(b[0]) {
		if len(b) == 0 {
			return Received{}, ErrBadCrypt
		}
		return Received{Data: b}, nil
	}
	random := b[1:3]
	a, ok := as4(ip)
	if !ok {
		return Received{}, ErrBadCrypt
	}
	try := int(b[0] & 3)
	if try == 3 {
		try = 1
	}
	var c *rc4.Cipher
	kad := false
	for range 3 {
		switch try {
		case 0:
			w := k.KadID.Wire()
			c, kad = rc4For(w[:], random), true
		case 1:
			c, kad = rc4For(k.UserHash[:], a[:], []byte{magicUDP}, random), false
		case 2:
			var vk [4]byte
			binary.LittleEndian.PutUint32(vk[:], k.VerifyKey(ip))
			c, kad = rc4For(vk[:], random), true
		}
		var magic [4]byte
		c.XORKeyStream(magic[:], b[3:7])
		if binary.LittleEndian.Uint32(magic[:]) == magicSyncClient {
			break
		}
		c = nil
		try = (try + 1) % 3
	}
	if c == nil {
		return Received{}, ErrBadCrypt
	}
	var pad [1]byte
	c.XORKeyStream(pad[:], b[7:8])
	rest := b[cryptHeaderLen:]
	if len(rest) <= int(pad[0]) {
		return Received{}, ErrBadCrypt
	}
	skip := make([]byte, pad[0])
	c.XORKeyStream(skip, rest[:pad[0]])
	rest = rest[pad[0]:]
	r := Received{Encrypted: true}
	if kad {
		if len(rest) <= 8 {
			return Received{}, ErrBadCrypt
		}
		var keys [8]byte
		c.XORKeyStream(keys[:], rest[:8])
		r.ValidKey = binary.LittleEndian.Uint32(keys[:4]) == k.VerifyKey(ip)
		r.SenderKey = binary.LittleEndian.Uint32(keys[4:])
		rest = rest[8:]
	}
	r.Data = make([]byte, len(rest))
	c.XORKeyStream(r.Data, rest)
	return r, nil
}

// EncryptKad obfuscates a Kad datagram for the node at ip, with its Kad ID
// when known (not zero), or else with the verify key it gave us
// (receiverKey). It returns the datagram unchanged when neither is known.
func (k *Keys) EncryptKad(data []byte, ip netip.Addr, target ID, receiverKey uint32) []byte {
	if _, ok := as4(ip); !ok {
		return data
	}
	var c *rc4.Cipher
	var random [2]byte
	rand.Read(random[:])
	var low byte
	switch {
	case !target.IsZero():
		w := target.Wire()
		c = rc4For(w[:], random[:])
	case receiverKey != 0:
		var rk [4]byte
		binary.LittleEndian.PutUint32(rk[:], receiverKey)
		c, low = rc4For(rk[:], random[:]), 2
	default:
		return data
	}
	out := make([]byte, 0, cryptHeaderLen+8+len(data))
	out = append(out, marker(func(m byte) byte { return m&^3 | low }), random[0], random[1])
	var hdr [13]byte
	binary.LittleEndian.PutUint32(hdr[0:], magicSyncClient)
	hdr[4] = 0 // no padding, as eMule
	binary.LittleEndian.PutUint32(hdr[5:], receiverKey)
	binary.LittleEndian.PutUint32(hdr[9:], k.VerifyKey(ip))
	c.XORKeyStream(hdr[:], hdr[:])
	out = append(out, hdr[:]...)
	body := make([]byte, len(data))
	c.XORKeyStream(body, data)
	return append(out, body...)
}

// EncryptClient obfuscates an eD2K client UDP datagram for the client with
// userHash, myIP being our public IP as that client sees it. While that IP
// is unknown (not IPv4), the datagram is returned unchanged.
func (k *Keys) EncryptClient(data []byte, userHash [16]byte, myIP netip.Addr) []byte {
	a, ok := as4(myIP)
	if !ok {
		return data
	}
	var random [2]byte
	rand.Read(random[:])
	c := rc4For(userHash[:], a[:], []byte{magicUDP}, random[:])
	out := make([]byte, 0, cryptHeaderLen+len(data))
	out = append(out, marker(func(m byte) byte { return m | 1 }), random[0], random[1])
	var hdr [5]byte
	binary.LittleEndian.PutUint32(hdr[0:], magicSyncClient)
	c.XORKeyStream(hdr[:], hdr[:])
	out = append(out, hdr[:]...)
	body := make([]byte, len(data))
	c.XORKeyStream(body, data)
	return append(out, body...)
}

// marker returns a random first byte, shaped by set, that is not a plain
// protocol byte.
func marker(set func(byte) byte) byte {
	var b [1]byte
	for range 128 {
		rand.Read(b[:])
		if m := set(b[0]); !isPlainMarker(m) {
			return m
		}
	}
	return set(0x01)
}
