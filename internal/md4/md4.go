// Package md4 implements the MD4 hash algorithm (RFC 1320).
//
// MD4 is cryptographically broken, but it is the hash the eD2K network
// uses to identify files and chunks, so we need it. Implemented here to
// keep the project free of external dependencies.
package md4

import (
	"encoding/binary"
	"hash"
	"math/bits"
)

const (
	Size      = 16
	BlockSize = 64
)

type digest struct {
	s   [4]uint32
	x   [BlockSize]byte
	nx  int
	len uint64
}

// New returns a new hash.Hash computing the MD4 checksum.
func New() hash.Hash {
	d := new(digest)
	d.Reset()
	return d
}

// Sum returns the MD4 checksum of data.
func Sum(data []byte) [Size]byte {
	d := new(digest)
	d.Reset()
	d.Write(data)
	var out [Size]byte
	copy(out[:], d.Sum(nil))
	return out
}

func (d *digest) Reset() {
	d.s = [4]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476}
	d.nx = 0
	d.len = 0
}

func (d *digest) Size() int      { return Size }
func (d *digest) BlockSize() int { return BlockSize }

func (d *digest) Write(p []byte) (int, error) {
	n := len(p)
	d.len += uint64(n)
	if d.nx > 0 {
		c := copy(d.x[d.nx:], p)
		d.nx += c
		p = p[c:]
		if d.nx == BlockSize {
			d.block(d.x[:])
			d.nx = 0
		}
	}
	for len(p) >= BlockSize {
		d.block(p[:BlockSize])
		p = p[BlockSize:]
	}
	if len(p) > 0 {
		d.nx = copy(d.x[:], p)
	}
	return n, nil
}

func (d *digest) Sum(in []byte) []byte {
	c := *d // don't modify the caller's state
	length := c.len
	var pad [72]byte
	pad[0] = 0x80
	padLen := 56 - int(length%64)
	if padLen <= 0 {
		padLen += 64
	}
	binary.LittleEndian.PutUint64(pad[padLen:], length<<3)
	c.Write(pad[:padLen+8])
	var out [Size]byte
	for i, v := range c.s {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return append(in, out[:]...)
}

var (
	shift1 = [4]int{3, 7, 11, 19}
	shift2 = [4]int{3, 5, 9, 13}
	shift3 = [4]int{3, 9, 11, 15}
	xIdx2  = [16]int{0, 4, 8, 12, 1, 5, 9, 13, 2, 6, 10, 14, 3, 7, 11, 15}
	xIdx3  = [16]int{0, 8, 4, 12, 2, 10, 6, 14, 1, 9, 5, 13, 3, 11, 7, 15}
)

func (d *digest) block(p []byte) {
	var x [16]uint32
	for i := range x {
		x[i] = binary.LittleEndian.Uint32(p[i*4:])
	}
	a, b, c, dd := d.s[0], d.s[1], d.s[2], d.s[3]

	for i := 0; i < 16; i++ {
		f := (b & c) | (^b & dd)
		a = bits.RotateLeft32(a+f+x[i], shift1[i%4])
		a, b, c, dd = dd, a, b, c
	}
	for i := 0; i < 16; i++ {
		g := (b & c) | (b & dd) | (c & dd)
		a = bits.RotateLeft32(a+g+x[xIdx2[i]]+0x5a827999, shift2[i%4])
		a, b, c, dd = dd, a, b, c
	}
	for i := 0; i < 16; i++ {
		h := b ^ c ^ dd
		a = bits.RotateLeft32(a+h+x[xIdx3[i]]+0x6ed9eba1, shift3[i%4])
		a, b, c, dd = dd, a, b, c
	}

	d.s[0] += a
	d.s[1] += b
	d.s[2] += c
	d.s[3] += dd
}
