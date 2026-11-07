// Package kad implements eMule's Kademlia network (Kad): a distributed
// hash table that finds sources and keywords without servers.
package kad

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"mymule/internal/proto"
)

// ID is a 128-bit Kad identifier: a node's ID, or the hash of a file or a
// keyword used as a key. ID[0] holds the most significant bits, so a file
// hash or an MD4 digest is used as an ID byte for byte.
type ID [16]byte

// IDLen is the size of an ID in bytes.
const IDLen = 16

// RandomID returns a random ID.
func RandomID() ID {
	var id ID
	rand.Read(id[:])
	return id
}

// ParseID reads 32 hex digits.
func ParseID(s string) (ID, bool) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != IDLen {
		return id, false
	}
	copy(id[:], b)
	return id, true
}

func (id ID) String() string { return strings.ToUpper(hex.EncodeToString(id[:])) }

// IsZero reports whether every bit is 0.
func (id ID) IsZero() bool { return id == ID{} }

// Xor returns the Kademlia distance between a and b.
func (a ID) Xor(b ID) ID {
	var d ID
	for i := range d {
		d[i] = a[i] ^ b[i]
	}
	return d
}

// Cmp compares a and b as numbers: -1, 0 or +1.
func (a ID) Cmp(b ID) int { return bytes.Compare(a[:], b[:]) }

// Bit returns bit i, 0 being the most significant.
func (id ID) Bit(i int) int {
	if i < 0 || i > 127 {
		return 0
	}
	return int(id[i/8]>>(7-uint(i%8))) & 1
}

// SetBit sets bit i (0 = most significant) to v (0 or 1).
func (id *ID) SetBit(i, v int) {
	if i < 0 || i > 127 {
		return
	}
	mask := byte(1) << (7 - uint(i%8))
	if v != 0 {
		id[i/8] |= mask
	} else {
		id[i/8] &^= mask
	}
}

// Chunk returns 32-bit chunk i (0 = most significant).
func (id ID) Chunk(i int) uint32 { return binary.BigEndian.Uint32(id[4*i:]) }

// WithPrefix returns an ID whose first n bits are those of id and whose
// other bits are random: a random ID in the part of the space id's first
// n bits describe.
func (id ID) WithPrefix(n int) ID {
	r := RandomID()
	for i := 0; i < n && i < 128; i++ {
		r.SetBit(i, id.Bit(i))
	}
	return r
}

// Wire is the ID as eMule sends it: four 32-bit numbers, most significant
// first, each in little-endian byte order. It is also the key that
// obfuscates packets sent to the node with this ID.
func (id ID) Wire() [16]byte {
	var b [16]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint32(b[4*i:], id.Chunk(i))
	}
	return b
}

// IDFromWire is the inverse of Wire.
func IDFromWire(b [16]byte) ID {
	var id ID
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(id[4*i:], binary.LittleEndian.Uint32(b[4*i:]))
	}
	return id
}

// WriteID writes id in wire order.
func WriteID(w *proto.Writer, id ID) {
	b := id.Wire()
	w.Bytes(b[:])
}

// ReadID reads an ID in wire order.
func ReadID(r *proto.Reader) ID { return IDFromWire(r.Hash()) }
