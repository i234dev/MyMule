package aich

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Recovery data proves the hashes of a part's blocks against the root:
// the hashes of the nodes beside the path from the root to the part, then
// the part's block hashes, each with its node identifier. As eMule writes
// it:
//
//	<count 2>{<ident 2><hash 20>} <count 2>{<ident 4><hash 20>}
//
// with the hashes in the first list (16-bit identifiers) for files up to
// 4 GB, in the second (32-bit) above.

// errRecovery is returned for recovery data that does not prove anything.
var errRecovery = errors.New("aich: invalid recovery data")

// CanRecover reports whether part p can have recovery data: eMule gives
// none for a file of one block or for a last part of one block or less.
func CanRecover(size uint64, p int) bool {
	start := uint64(p) * PartSize
	return size > BlockSize && start < size && size-start > BlockSize
}

// Recovery builds the recovery data of part p, given the hashes of its
// blocks (from the data).
func (t *Tree) Recovery(p int, blocks []Hash) ([]byte, error) {
	if !CanRecover(t.Size, p) || p >= len(t.Parts) {
		return nil, fmt.Errorf("aich: no recovery data for part %d", p)
	}
	target, err := partNode(t.Size, p)
	if err != nil {
		return nil, err
	}
	if ph, err := PartHash(t.Size, p, blocks); err != nil || ph != t.Parts[p] {
		return nil, fmt.Errorf("aich: the data of part %d does not match its hash", p)
	}
	type entry struct {
		ident uint32
		h     Hash
	}
	var list []entry
	// The siblings, from the top.
	n := root(t.Size)
	for n.size > PartSize {
		l, r := n.children()
		if target.start < r.start {
			list = append(list, entry{r.ident, t.above(r)})
			n = l
		} else {
			list = append(list, entry{l.ident, t.above(l)})
			n = r
		}
	}
	// The part's blocks, in order.
	var leaves func(n node, blocks []Hash)
	leaves = func(n node, blocks []Hash) {
		if n.leaf() {
			list = append(list, entry{n.ident, blocks[0]})
			return
		}
		l, r := n.children()
		leaves(l, blocks)
		leaves(r, blocks[(l.size+BlockSize-1)/BlockSize:])
	}
	leaves(target, blocks)

	wide := t.Size > largeFile
	var out []byte
	if wide {
		out = binary.LittleEndian.AppendUint16(out, 0)
	}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(list)))
	for _, e := range list {
		if wide {
			out = binary.LittleEndian.AppendUint32(out, e.ident)
		} else {
			out = binary.LittleEndian.AppendUint16(out, uint16(e.ident))
		}
		out = append(out, e.h[:]...)
	}
	if !wide {
		out = binary.LittleEndian.AppendUint16(out, 0)
	}
	return out, nil
}

// expected is how many hashes the recovery data of a part holds.
func expected(size uint64, target node) int {
	return target.depth + int((target.size+BlockSize-1)/BlockSize)
}

// Verify checks the recovery data of part p of a file of size bytes
// against the file's root, and returns the hashes of the part's blocks.
// It reads data as eMule writes it, and takes only data that proves the
// root: exactly the hashes needed, each once.
func Verify(size uint64, p int, want Hash, data []byte) ([]Hash, error) {
	if !CanRecover(size, p) {
		return nil, fmt.Errorf("aich: no recovery data for part %d", p)
	}
	target, err := partNode(size, p)
	if err != nil {
		return nil, err
	}
	count := expected(size, target)
	got := map[uint32]Hash{}
	read := func(wide bool) error {
		if len(data) < 2 {
			return errRecovery
		}
		n := int(binary.LittleEndian.Uint16(data))
		data = data[2:]
		w := 2
		if wide {
			w = 4
		}
		if n != 0 && n != count || len(data) < n*(w+20) {
			return errRecovery
		}
		for range n {
			var id uint32
			if wide {
				id = binary.LittleEndian.Uint32(data)
			} else {
				id = uint32(binary.LittleEndian.Uint16(data))
			}
			if _, dup := got[id]; dup || id <= 1 {
				return errRecovery
			}
			var h Hash
			copy(h[:], data[w:w+20])
			got[id] = h
			data = data[w+20:]
		}
		return nil
	}
	if err := read(false); err != nil {
		return nil, err
	}
	if len(got) == 0 {
		if err := read(true); err != nil {
			return nil, err
		}
	}
	if len(got) != count {
		return nil, errRecovery
	}

	// Walk down to the part, taking the siblings; then up from the blocks.
	var siblings []Hash
	var path []node
	n := root(size)
	for n.size > PartSize {
		l, r := n.children()
		path = append(path, n)
		var sib node
		if target.start < r.start {
			n, sib = l, r
		} else {
			n, sib = r, l
		}
		h, ok := got[sib.ident]
		if !ok {
			return nil, errRecovery
		}
		siblings = append(siblings, h)
	}
	var blocks []Hash
	var leaves func(n node) bool
	leaves = func(n node) bool {
		if n.leaf() {
			h, ok := got[n.ident]
			blocks = append(blocks, h)
			return ok
		}
		l, r := n.children()
		return leaves(l) && leaves(r)
	}
	if !leaves(target) {
		return nil, errRecovery
	}
	h := subtree(target, blocks)
	for i := len(path) - 1; i >= 0; i-- {
		l, _ := path[i].children()
		if target.start < l.start+l.size {
			h = join(h, siblings[i])
		} else {
			h = join(siblings[i], h)
		}
	}
	if h != want {
		return nil, errors.New("aich: recovery data does not match the file's AICH hash")
	}
	return blocks, nil
}
