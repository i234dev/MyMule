// Package obfus implements eMule's protocol obfuscation for client
// connections over TCP: both ends derive RC4 keys from the receiving
// client's user hash and a random value, and everything after a short
// handshake looks like random bytes. It hides eD2K traffic from simple
// traffic shaping; it does not make anyone anonymous, and anyone who
// knows the user hash can read it.
//
// The connection that starts it (A) sends:
//
//	<marker 1><random key part 4> unencrypted, then encrypted
//	<magic 4><methods supported 1><method preferred 1><padding length 1><padding>
//
// and the receiver (B) answers, encrypted:
//
//	<magic 4><method selected 1><padding length 1><padding>
//
// A sends with MD5(hash of B, 34, random part) and receives with
// MD5(hash of B, 203, random part); RC4, the first 1024 bytes dropped.
package obfus

import (
	"bufio"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"time"

	"mymule/internal/ed2k"
)

const (
	magicSync      = 0x835E6FC4
	magicRequester = 34
	magicServer    = 203
	methodObfus    = 0
	maxPadding     = 16 // we send 0-15 bytes; any length up to 255 is read

	protoEDonkey = 0xE3
	protoEMule   = 0xC5
	protoPacked  = 0xD4
)

// Crypt options, as servers, Kad and hellos give them.
const (
	Supports = 0x01
	Requests = 0x02
	Requires = 0x04
)

// ErrNotObfuscated is returned by Accept when a peer connected without
// obfuscation and that is not allowed.
var ErrNotObfuscated = errors.New("connection not obfuscated")

var errHandshake = errors.New("obfuscation handshake failed")

// HandshakeTimeout bounds the whole handshake.
var HandshakeTimeout = 20 * time.Second

func newCipher(hash ed2k.Hash, magic byte, random []byte) *rc4.Cipher {
	b := make([]byte, 0, 21)
	b = append(append(append(b, hash[:]...), magic), random...)
	key := md5.Sum(b)
	c, _ := rc4.NewCipher(key[:])
	var drop [1024]byte
	c.XORKeyStream(drop[:], drop[:])
	return c
}

// Conn is an obfuscated connection.
type Conn struct {
	net.Conn
	r          io.Reader
	in, out    *rc4.Cipher
	writeBuf   []byte
	obfuscated bool
}

// Obfuscated reports whether the connection is obfuscated (Accept also
// returns plain connections).
func (c *Conn) Obfuscated() bool { return c.obfuscated }

func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if c.in != nil {
		c.in.XORKeyStream(b[:n], b[:n])
	}
	return n, err
}

// Write encrypts b into a buffer of its own: the caller's bytes stay
// as they are.
func (c *Conn) Write(b []byte) (int, error) {
	if c.out == nil {
		return c.Conn.Write(b)
	}
	if cap(c.writeBuf) < len(b) {
		c.writeBuf = make([]byte, len(b))
	}
	buf := c.writeBuf[:len(b)]
	c.out.XORKeyStream(buf, b)
	n, err := c.Conn.Write(buf)
	if n < len(b) && err == nil {
		err = io.ErrShortWrite
	}
	// The stream cipher moved past all of b: a short write leaves the
	// connection unusable, as any write error does.
	return n, err
}

func padding() []byte {
	var n [1]byte
	rand.Read(n[:])
	p := make([]byte, int(n[0])%maxPadding)
	rand.Read(p)
	return p
}

// Client starts an obfuscated connection on nc to the peer whose user
// hash is peerHash.
func Client(nc net.Conn, peerHash ed2k.Hash) (*Conn, error) {
	nc.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer nc.SetDeadline(time.Time{})
	var head [5]byte
	rand.Read(head[:])
	for head[0] == protoEDonkey || head[0] == protoEMule || head[0] == protoPacked {
		rand.Read(head[:1])
	}
	c := &Conn{Conn: nc, r: bufio.NewReader(nc), obfuscated: true,
		out: newCipher(peerHash, magicRequester, head[1:]),
		in:  newCipher(peerHash, magicServer, head[1:])}
	pad := padding()
	req := binary.LittleEndian.AppendUint32(nil, magicSync)
	req = append(req, methodObfus, methodObfus, byte(len(pad)))
	req = append(req, pad...)
	c.out.XORKeyStream(req, req)
	if _, err := nc.Write(append(head[:], req...)); err != nil {
		return nil, err
	}
	var ans [6]byte
	if _, err := io.ReadFull(c, ans[:]); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(ans[:]) != magicSync || ans[4] != methodObfus {
		return nil, errHandshake
	}
	if _, err := io.CopyN(io.Discard, c, int64(ans[5])); err != nil {
		return nil, err
	}
	return c, nil
}

// Accept reads how a peer that connected to us starts: plain eD2K, or
// the obfuscation handshake for our user hash ourHash, which it then
// completes. A plain connection is refused with ErrNotObfuscated unless
// plainOK; an obfuscated one too unless obfusOK.
func Accept(nc net.Conn, ourHash ed2k.Hash, plainOK, obfusOK bool) (*Conn, error) {
	nc.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer nc.SetDeadline(time.Time{})
	br := bufio.NewReader(nc)
	first, err := br.Peek(1)
	if err != nil {
		return nil, err
	}
	switch first[0] {
	case protoEDonkey, protoEMule, protoPacked:
		if !plainOK {
			return nil, ErrNotObfuscated
		}
		return &Conn{Conn: nc, r: br}, nil
	}
	if !obfusOK {
		return nil, errHandshake
	}
	var head [5]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return nil, err
	}
	c := &Conn{Conn: nc, r: br, obfuscated: true,
		in:  newCipher(ourHash, magicRequester, head[1:]),
		out: newCipher(ourHash, magicServer, head[1:])}
	var req [7]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(req[:]) != magicSync {
		return nil, errHandshake // not for us, or not obfuscation at all
	}
	if _, err := io.CopyN(io.Discard, c, int64(req[6])); err != nil {
		return nil, err
	}
	pad := padding()
	ans := binary.LittleEndian.AppendUint32(nil, magicSync)
	ans = append(ans, methodObfus, byte(len(pad)))
	ans = append(ans, pad...)
	if _, err := c.Write(ans); err != nil {
		return nil, err
	}
	return c, nil
}

// The obfuscation of server connections agrees on the keys with
// Diffie-Hellman: g = 2, this 768-bit prime, 128-bit secret exponents.
var dhPrime = new(big.Int).SetBytes([]byte{
	0xF2, 0xBF, 0x52, 0xC5, 0x5F, 0x58, 0x7A, 0xDD, 0x53, 0x71, 0xA9, 0x36,
	0xE8, 0x86, 0xEB, 0x3C, 0x62, 0x17, 0xA3, 0x3E, 0xC3, 0x4C, 0xB4, 0x0D,
	0xC7, 0x3A, 0x41, 0xA6, 0x43, 0xAF, 0xFC, 0xE7, 0x21, 0xFC, 0x28, 0x63,
	0x66, 0x53, 0x5B, 0xDB, 0xCE, 0x25, 0x9F, 0x22, 0x86, 0xDA, 0x4A, 0x91,
	0xB2, 0x07, 0xCB, 0xAA, 0x52, 0x55, 0xD4, 0xF6, 0x1C, 0xCE, 0xAE, 0xD4,
	0x5A, 0xD5, 0xE0, 0x74, 0x7D, 0xF7, 0x78, 0x18, 0x28, 0x10, 0x5F, 0x34,
	0x0F, 0x76, 0x23, 0x87, 0xF8, 0x8B, 0x28, 0x91, 0x42, 0xFB, 0x42, 0x68,
	0x8F, 0x05, 0x15, 0x0F, 0x54, 0x8B, 0x5F, 0x43, 0x6A, 0xF7, 0x0D, 0xF3,
})

const primeSize = 96

func serverCiphers(s *big.Int) (send, recv *rc4.Cipher) {
	b := make([]byte, primeSize+1)
	s.FillBytes(b[:primeSize])
	mk := func(magic byte) *rc4.Cipher {
		b[primeSize] = magic
		key := md5.Sum(b)
		c, _ := rc4.NewCipher(key[:])
		var drop [1024]byte
		c.XORKeyStream(drop[:], drop[:])
		return c
	}
	return mk(magicRequester), mk(magicServer)
}

// Server starts an obfuscated connection to a server, on nc connected to
// the server's obfuscation port:
//
//	-> <marker 1><g^a 96><padding length 1><padding>          unencrypted
//	<- <g^b 96> then encrypted <magic 4><methods 1><preferred 1><padding length 1><padding>
//	-> encrypted <magic 4><method selected 1><padding length 1><padding>
//
// The keys are MD5(s, 34) to send and MD5(s, 203) to receive, s being
// the shared secret in 96 bytes.
func Server(nc net.Conn) (*Conn, error) {
	nc.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer nc.SetDeadline(time.Time{})
	ab := make([]byte, 16)
	rand.Read(ab)
	a := new(big.Int).SetBytes(ab)
	ga := new(big.Int).Exp(big.NewInt(2), a, dhPrime)
	var marker [1]byte
	rand.Read(marker[:])
	for marker[0] == protoEDonkey || marker[0] == protoEMule || marker[0] == protoPacked {
		rand.Read(marker[:])
	}
	pad := padding()
	req := append(append(marker[:], ga.FillBytes(make([]byte, primeSize))...), byte(len(pad)))
	if _, err := nc.Write(append(req, pad...)); err != nil {
		return nil, err
	}
	br := bufio.NewReader(nc)
	gb := make([]byte, primeSize)
	if _, err := io.ReadFull(br, gb); err != nil {
		return nil, err
	}
	b := new(big.Int).SetBytes(gb)
	if b.Cmp(big.NewInt(1)) <= 0 || b.Cmp(new(big.Int).Sub(dhPrime, big.NewInt(1))) >= 0 {
		return nil, errHandshake
	}
	c := &Conn{Conn: nc, r: br, obfuscated: true}
	c.out, c.in = serverCiphers(new(big.Int).Exp(b, a, dhPrime))
	var ans [7]byte
	if _, err := io.ReadFull(c, ans[:]); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(ans[:]) != magicSync {
		return nil, errHandshake
	}
	if _, err := io.CopyN(io.Discard, c, int64(ans[6])); err != nil {
		return nil, err
	}
	pad = padding()
	fin := binary.LittleEndian.AppendUint32(nil, magicSync)
	fin = append(append(fin, methodObfus, byte(len(pad))), pad...)
	if _, err := c.Write(fin); err != nil {
		return nil, err
	}
	return c, nil
}
