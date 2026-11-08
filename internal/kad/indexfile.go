package kad

import (
	"errors"
	"net/netip"
	"time"

	"mymule/internal/proto"
)

// kadindex.dat keeps what others published to us across restarts, as
// eMule's key_index.dat and src_index.dat (notes are not kept, as in
// eMule). Keyword entries are only kept for the same Kad ID: with another
// ID we are no longer the node for those keywords.

const indexFile = "kadindex.dat"

// maxIndexFile is the largest kadindex.dat read.
const maxIndexFile = 256 << 20

func writeEntry(w *proto.Writer, e *entry, keyword bool) {
	WriteID(w, e.key)
	WriteID(w, e.source)
	a := e.ip.Unmap().As4()
	w.Bytes(a[:])
	w.U16(e.udpPort)
	w.U16(e.tcpPort)
	w.U64(e.size)
	w.U64(uint64(e.expires.Unix()))
	if keyword {
		w.U16(uint16(len(e.names)))
		for _, n := range e.names {
			w.Str16(n.name)
			w.U32(uint32(n.popularity))
		}
		w.U16(uint16(len(e.publishers)))
		for _, p := range e.publishers {
			a := p.ip.Unmap().As4()
			w.Bytes(a[:])
			w.U64(uint64(p.last.Unix()))
			if p.weak {
				w.U8(1)
			} else {
				w.U8(0)
			}
		}
	}
	WriteTags(w, e.tags)
}

func readEntry(r *proto.Reader, keyword bool) (*entry, error) {
	e := &entry{key: ReadID(r), source: ReadID(r)}
	var a [4]byte
	copy(a[:], r.Bytes(4))
	e.ip = netip.AddrFrom4(a)
	e.udpPort = r.U16()
	e.tcpPort = r.U16()
	e.size = r.U64()
	e.expires = time.Unix(int64(r.U64()), 0)
	if keyword {
		nn := int(r.U16())
		if nn > maxNamesPerEntry {
			return nil, errors.New("too many names")
		}
		for range nn {
			name := r.Str16()
			pop := int(r.U32())
			e.names = append(e.names, fileName{name: name, popularity: pop})
		}
		np := int(r.U16())
		if np > maxPublishersPerEntry {
			return nil, errors.New("too many publishers")
		}
		for range np {
			copy(a[:], r.Bytes(4))
			last := time.Unix(int64(r.U64()), 0)
			e.publishers = append(e.publishers, publisher{ip: netip.AddrFrom4(a), last: last, weak: r.U8() != 0})
		}
	}
	tags, err := ReadTags(r)
	if err != nil {
		return nil, err
	}
	e.tags = keepTags(tags)
	return e, r.Err()
}

// encode writes the index for kadindex.dat.
func (x *index) encode(me ID, now time.Time) []byte {
	var w proto.Writer
	w.U32(1)
	WriteID(&w, me)
	var kw []*entry
	for _, files := range x.keywords {
		for _, list := range files {
			kw = append(kw, list...)
		}
	}
	w.U32(uint32(len(kw)))
	for _, e := range kw {
		writeEntry(&w, e, true)
	}
	var src []*entry
	for _, list := range x.sources {
		src = append(src, list...)
	}
	w.U32(uint32(len(src)))
	for _, e := range src {
		writeEntry(&w, e, false)
	}
	w.U32(uint32(len(x.load)))
	for id, t := range x.load {
		WriteID(&w, id)
		w.U64(uint64(t.Unix()))
	}
	return w.B
}

// decode reads kadindex.dat into an empty index, keeping only what has
// not expired and fits the limits.
func (x *index) decode(b []byte, me ID, now time.Time) error {
	if len(b) > maxIndexFile {
		return errors.New("kadindex.dat too big")
	}
	r := proto.NewReader(b)
	if r.U32() != 1 {
		return errors.New("unknown kadindex.dat version")
	}
	sameID := ReadID(r) == me
	n := int(r.U32())
	if r.Err() != nil || n < 0 || n > r.Len()/50 {
		return errors.New("kadindex.dat truncated")
	}
	for range n {
		e, err := readEntry(r, true)
		if err != nil {
			return err
		}
		if sameID && e.expires.After(now) && e.size > 0 && e.name() != "" {
			x.insertKeyword(e)
		}
	}
	n = int(r.U32())
	if r.Err() != nil || n < 0 || n > r.Len()/50 {
		return errors.New("kadindex.dat truncated")
	}
	for range n {
		e, err := readEntry(r, false)
		if err != nil {
			return err
		}
		if e.expires.After(now) {
			x.addSource(e)
		}
	}
	n = int(r.U32())
	if r.Err() != nil || n < 0 || n > r.Len()/24 {
		return errors.New("kadindex.dat truncated")
	}
	for range n {
		id := ReadID(r)
		t := time.Unix(int64(r.U64()), 0)
		if t.After(now) {
			x.load[id] = t
		}
	}
	return r.Err()
}

// insertKeyword adds a saved keyword entry as it was.
func (x *index) insertKeyword(e *entry) {
	if x.nKeyword >= maxKeywordEntries {
		return
	}
	files := x.keywords[e.key]
	if (files == nil && len(x.keywords) >= maxKeys) || len(files) >= maxFilesPerKeyword {
		return
	}
	if len(files[e.source]) >= maxSizesPerFile || x.ownerFull(e.ip) {
		return
	}
	for _, o := range files[e.source] {
		if o.size == e.size {
			return
		}
	}
	for i := range e.names {
		e.names[i].name = cutName(e.names[i].name)
	}
	e.settle()
	if !x.room(e.cost) {
		return
	}
	if files == nil {
		files = map[ID][]*entry{}
		x.keywords[e.key] = files
	}
	x.bytes += e.cost
	files[e.source] = append(files[e.source], e)
	x.nKeyword++
	e.owner = e.ip // the last publisher is all the file says
	x.own(e.owner, 1)
	for _, p := range e.publishers {
		if !p.weak {
			x.pubSubnets[subnet(p.ip)]++
		}
	}
}
