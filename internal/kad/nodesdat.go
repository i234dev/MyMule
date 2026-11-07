package kad

import (
	"encoding/binary"
	"errors"
	"net/netip"

	"mymule/internal/proto"
)

// nodes.dat, eMule's list of known Kad nodes:
//
//	[u32 0][u32 version][u32 count] then per node (25 bytes, plus 9 from
//	version 2): [ID][u32 ip][u16 udp][u16 tcp][u8 kad version]
//	[u32 verify key][key's IP, network order][u8 verified]
//
// Version 3 with a u32 1 after the version is a "bootstrap" nodes.dat:
// nodes to bootstrap from, not to keep. A file starting with a non-zero
// count is the old version 0 format, which eMule no longer reads either.

// NodesFile is a parsed nodes.dat.
type NodesFile struct {
	Contacts  []Contact
	Bootstrap bool // a bootstrap list: contact these, do not keep them
	// HasVerified is false for files that did not save the verified flag
	// (or saved none): then every contact counts as verified.
	HasVerified bool
}

// MaxNodesFile is the largest nodes.dat read.
const MaxNodesFile = 1 << 20

// ipFromU32 turns an IP in eMule's Kad order (a.b.c.d = a<<24|...) into
// an address.
func ipFromU32(v uint32) netip.Addr {
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], v)
	return netip.AddrFrom4(a)
}

func ipToU32(ip netip.Addr) uint32 {
	a := ip.Unmap().As4()
	return binary.BigEndian.Uint32(a[:])
}

// ParseNodes reads a nodes.dat.
func ParseNodes(b []byte) (NodesFile, error) {
	var f NodesFile
	if len(b) > MaxNodesFile {
		return f, errors.New("kad: nodes.dat too big")
	}
	r := proto.NewReader(b)
	if r.U32() != 0 {
		return f, errors.New("kad: nodes.dat in the old version 0 format")
	}
	version := r.U32()
	if r.Err() != nil || version < 1 || version > 3 {
		return f, errors.New("kad: unknown nodes.dat version")
	}
	if version == 3 && r.U32() == 1 {
		f.Bootstrap = true
	}
	n := int(r.U32())
	size := 25
	if version >= 2 && !f.Bootstrap {
		size = 34
	}
	if r.Err() != nil || n < 0 || n > r.Len()/size {
		return f, errors.New("kad: nodes.dat truncated")
	}
	for range n {
		c := Contact{ID: ReadID(r)}
		c.IP = ipFromU32(r.U32())
		c.UDPPort = r.U16()
		c.TCPPort = r.U16()
		c.Version = r.U8()
		if size == 34 {
			key := r.U32()
			// The key's IP is stored in network order, unlike the node's.
			var a [4]byte
			copy(a[:], r.Bytes(4))
			if key != 0 && a != [4]byte{} {
				c.Key = UDPKey{Key: key, IP: netip.AddrFrom4(a)}
			}
			c.Verified = r.U8() != 0
			f.HasVerified = f.HasVerified || c.Verified
		}
		f.Contacts = append(f.Contacts, c)
	}
	return f, r.Err()
}

// EncodeNodes writes contacts as a version 2 nodes.dat.
func EncodeNodes(cs []Contact) []byte {
	var w proto.Writer
	w.U32(0)
	w.U32(2)
	w.U32(uint32(len(cs)))
	for _, c := range cs {
		WriteID(&w, c.ID)
		w.U32(ipToU32(c.IP))
		w.U16(c.UDPPort)
		w.U16(c.TCPPort)
		w.U8(c.Version)
		if c.Key.IP.Unmap().Is4() {
			w.U32(c.Key.Key)
			a := c.Key.IP.Unmap().As4()
			w.Bytes(a[:])
		} else {
			w.U32(0)
			w.U32(0)
		}
		if c.Verified {
			w.U8(1)
		} else {
			w.U8(0)
		}
	}
	return w.B
}
