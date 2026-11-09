// Package aich computes eMule's AICH hashes (Advanced Intelligent
// Corruption Handling): a SHA-1 tree over 180 KB blocks whose root names
// a file's content. With it, a part that fails its eD2K hash check need
// not be downloaded again whole: a peer sends the hashes of that part's
// blocks ("recovery data"), checked against the root, and only the bad
// blocks are downloaded again.
package aich

import (
	"crypto/sha1"
	"encoding/base32"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Sizes, as eMule's.
const (
	BlockSize = 184320  // a leaf of the tree
	PartSize  = 9728000 // an eD2K part
	// blocksPerPart is how many blocks a whole part holds (the last one
	// short).
	blocksPerPart = (PartSize + BlockSize - 1) / BlockSize
	// largeFile: above it, recovery data uses 32-bit node identifiers.
	largeFile = 4290048000
)

// Hash is an AICH hash (SHA-1).
type Hash [20]byte

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// String is the hash as in ed2k links (base32).
func (h Hash) String() string { return b32.EncodeToString(h[:]) }

// IsZero reports whether h is unset.
func (h Hash) IsZero() bool { return h == Hash{} }

// ParseHash reads a hash as String writes it.
func ParseHash(s string) (Hash, error) {
	var h Hash
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 32 {
		return h, errors.New("invalid AICH hash")
	}
	b, err := b32.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, errors.New("invalid AICH hash")
	}
	copy(h[:], b)
	return h, nil
}

// MarshalText and UnmarshalText store a hash as String writes it.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

func (h *Hash) UnmarshalText(b []byte) error {
	v, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = v
	return nil
}

func join(a, b Hash) Hash {
	s := sha1.New()
	s.Write(a[:])
	s.Write(b[:])
	var h Hash
	s.Sum(h[:0])
	return h
}

// Block returns the hash of one block of data.
func Block(data []byte) Hash { return sha1.Sum(data) }

// split returns the sizes of the two halves of a node of size bytes. A
// node above PartSize splits on part boundaries, one at most on block
// boundaries; a left node's left half takes the odd unit, a right
// node's right half does.
func split(size uint64, left bool) (uint64, uint64) {
	base := uint64(BlockSize)
	if size > PartSize {
		base = PartSize
	}
	units := (size + base - 1) / base
	if left {
		units++
	}
	l := units / 2 * base
	return l, size - l
}

// node is a node of the tree: the bytes it covers, whether it is a left
// child (the root counts as one), and its identifier in recovery data (1
// for the root, then a bit per level: 1 left, 0 right).
type node struct {
	start, size uint64
	left        bool
	ident       uint32
	depth       int
}

func (n node) children() (node, node) {
	l, r := split(n.size, n.left)
	return node{n.start, l, true, n.ident<<1 | 1, n.depth + 1},
		node{n.start + l, r, false, n.ident << 1, n.depth + 1}
}

func (n node) leaf() bool { return n.size <= BlockSize }

func root(size uint64) node { return node{0, size, true, 1, 0} }

// partNode finds the node of part p (nodes of PartSize or less are the
// parts).
func partNode(size uint64, p int) (node, error) {
	start := uint64(p) * PartSize
	if size == 0 || start >= size {
		return node{}, fmt.Errorf("aich: no part %d", p)
	}
	n := root(size)
	for n.size > PartSize {
		l, r := n.children()
		if start < r.start {
			n = l
		} else {
			n = r
		}
	}
	return n, nil
}

// subtree computes the hash of n from the hashes of its blocks, in order
// (blocks[0] is n's first block).
func subtree(n node, blocks []Hash) Hash {
	if n.leaf() {
		return blocks[0]
	}
	l, r := n.children()
	return join(subtree(l, blocks), subtree(r, blocks[(l.size+BlockSize-1)/BlockSize:]))
}

// Tree is what is kept of a file's tree: the hash of each part. Block
// hashes are computed again from the data when needed.
type Tree struct {
	Size  uint64
	Parts []Hash
}

// Root returns the file's AICH hash.
func (t *Tree) Root() Hash { return t.above(root(t.Size)) }

// above computes the hash of a node of a part or bigger.
func (t *Tree) above(n node) Hash {
	if n.size <= PartSize {
		return t.Parts[n.start/PartSize]
	}
	l, r := n.children()
	return join(t.above(l), t.above(r))
}

// PartBlocks returns the number of blocks in part p of a file of size
// bytes.
func PartBlocks(size uint64, p int) int {
	start := uint64(p) * PartSize
	if start >= size {
		return 0
	}
	return int((min(PartSize, size-start) + BlockSize - 1) / BlockSize)
}

// PartHash computes the hash of part p from the hashes of its blocks.
func PartHash(size uint64, p int, blocks []Hash) (Hash, error) {
	n, err := partNode(size, p)
	if err != nil {
		return Hash{}, err
	}
	if len(blocks) != PartBlocks(size, p) {
		return Hash{}, fmt.Errorf("aich: part %d has %d blocks, not %d", p, PartBlocks(size, p), len(blocks))
	}
	return subtree(n, blocks), nil
}

// Hasher computes a file's tree from its data, written in order.
type Hasher struct {
	size   uint64
	off    uint64 // bytes written
	sha    hash.Hash
	inBlk  uint64 // bytes of the current block
	blocks []Hash // of the current part
	tree   Tree
	err    error
}

// NewHasher returns a hasher for a file of size bytes.
func NewHasher(size uint64) *Hasher {
	return &Hasher{size: size, sha: sha1.New(), tree: Tree{Size: size}}
}

// blockEnd is where the block holding off ends (blocks never cross a part
// boundary).
func (h *Hasher) blockEnd(off uint64) uint64 {
	partEnd := min((off/PartSize+1)*PartSize, h.size)
	return min(off-off%PartSize+((off%PartSize)/BlockSize+1)*BlockSize, partEnd)
}

func (h *Hasher) Write(p []byte) (int, error) {
	if h.err != nil {
		return 0, h.err
	}
	if h.off+uint64(len(p)) > h.size {
		h.err = errors.New("aich: more data than the file size")
		return 0, h.err
	}
	n := len(p)
	for len(p) > 0 {
		end := h.blockEnd(h.off)
		take := min(uint64(len(p)), end-h.off)
		h.sha.Write(p[:take])
		h.off += take
		p = p[take:]
		if h.off == end {
			var b Hash
			h.sha.Sum(b[:0])
			h.sha.Reset()
			h.blocks = append(h.blocks, b)
			if h.off%PartSize == 0 || h.off == h.size {
				part := int((h.off - 1) / PartSize)
				ph, err := PartHash(h.size, part, h.blocks)
				if err != nil {
					h.err = err
					return 0, err
				}
				h.tree.Parts = append(h.tree.Parts, ph)
				h.blocks = h.blocks[:0]
			}
		}
	}
	return n, nil
}

// Tree returns the tree, once all the data is written.
func (h *Hasher) Tree() (*Tree, error) {
	if h.err != nil {
		return nil, h.err
	}
	if h.off != h.size || h.size == 0 {
		return nil, fmt.Errorf("aich: %d of %d bytes written", h.off, h.size)
	}
	t := h.tree
	t.Parts = append([]Hash(nil), t.Parts...)
	return &t, nil
}

// FromReader computes the tree of size bytes read from r.
func FromReader(r io.Reader, size uint64) (*Tree, error) {
	h := NewHasher(size)
	if _, err := io.CopyN(h, r, int64(size)); err != nil {
		return nil, err
	}
	return h.Tree()
}

// BlockHashes hashes the blocks of part p, read with read (which fills
// the buffer at the offset in the file, or fails).
func BlockHashes(size uint64, p int, read func(buf []byte, off int64) error) ([]Hash, error) {
	n := PartBlocks(size, p)
	if n == 0 {
		return nil, fmt.Errorf("aich: no part %d", p)
	}
	start := uint64(p) * PartSize
	end := min(start+PartSize, size)
	buf := make([]byte, BlockSize)
	out := make([]Hash, 0, n)
	for off := start; off < end; off += BlockSize {
		b := buf[:min(BlockSize, end-off)]
		if err := read(b, int64(off)); err != nil {
			return nil, err
		}
		out = append(out, Block(b))
	}
	return out, nil
}
