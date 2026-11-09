package peer

import (
	"errors"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// eD2K client UDP (eMule's): a peer waiting in an upload queue asks for
// its place with a small datagram instead of a new TCP connection.
//
//	OP_REASKFILEPING <file hash 16>[<part status> from version 4]
//	                 [<complete sources 2> from version 3]
//	OP_REASKACK      [<part status> from version 4]<queue rank 2>
//	OP_FILENOTFOUND  (nothing)
//	OP_QUEUEFULL     (nothing)
//
// A part status is <count 2><bits>, as in OP_FILESTATUS; the version is
// the one of the peer that receives the ping, from its hello (misc
// options 1, bits 24-27).
const (
	OpReaskFilePing = 0x90
	OpReaskAck      = 0x91
	OpFileNotFound  = 0x92
	OpQueueFull     = 0x93
)

// UDPVersion is the client UDP version we speak.
const UDPVersion = 4

// UDPVersion is the client UDP version the peer speaks (0 = none).
func (h Hello) UDPVersion() byte { return byte(h.Tags.Int(CTEMuleMiscOpts1)>>24) & 0x0F }

// UDPPort is the peer's eD2K client UDP port (0 = none).
func (h Hello) UDPPort() uint16 { return uint16(h.Tags.Int(CTEMuleUDPPorts)) }

// WithClientUDP makes a hello we send offer client UDP on port (the
// low 16 bits of the UDP ports tag; the Kad port stays) and our version.
func (h Hello) WithClientUDP(port uint16) Hello {
	h = h.WithUDPPort(port)
	tags := make(proto.TagList, 0, len(h.Tags))
	for _, t := range h.Tags {
		if t.Name == "" && t.ID == CTEMuleMiscOpts1 {
			t.Int = t.Int&^(0x0F<<24) | uint64(UDPVersion)<<24
		}
		tags = append(tags, t)
	}
	h.Tags = tags
	return h
}

// writeStatus writes a part status: nil = complete or unknown (count 0).
func writeStatus(w *proto.Writer, status []bool) {
	w.U16(uint16(len(status)))
	if len(status) == 0 {
		return
	}
	b := make([]byte, (len(status)+7)/8)
	for i, v := range status {
		if v {
			b[i/8] |= 1 << (i % 8)
		}
	}
	w.Bytes(b)
}

// maxStatusParts bounds a part status read (a 256 GiB file has about
// 28,000 parts).
const maxStatusParts = 32000

func readStatus(r *proto.Reader) ([]bool, error) {
	n := int(r.U16())
	if n == 0 {
		return nil, r.Err()
	}
	if n > maxStatusParts {
		return nil, errBadUDP
	}
	b := r.Bytes((n + 7) / 8)
	if r.Err() != nil {
		return nil, errBadUDP
	}
	out := make([]bool, n)
	for i := range out {
		out[i] = b[i/8]&(1<<(i%8)) != 0
	}
	return out, nil
}

var errBadUDP = errors.New("bad client UDP packet")

// EncodeReaskFilePing builds OP_REASKFILEPING for a peer of client UDP
// version v: our part status of the file (nil for complete or unknown)
// and how many complete sources we know.
func EncodeReaskFilePing(v byte, file ed2k.Hash, status []bool, complete uint16) []byte {
	var w proto.Writer
	w.Bytes(file[:])
	if v >= 4 {
		writeStatus(&w, status)
	}
	if v >= 3 {
		w.U16(complete)
	}
	return w.B
}

// DecodeReaskFilePing reads OP_REASKFILEPING from a peer of client UDP
// version v (what follows the hash is only read, not needed).
func DecodeReaskFilePing(payload []byte, v byte) (ed2k.Hash, error) {
	r := proto.NewReader(payload)
	file := ed2k.Hash(r.Hash())
	if r.Err() != nil {
		return file, errBadUDP
	}
	if v >= 4 {
		if _, err := readStatus(r); err != nil {
			return file, err
		}
	}
	return file, nil
}

// EncodeReaskAck builds OP_REASKACK for a peer of client UDP version v.
func EncodeReaskAck(v byte, status []bool, rank uint16) []byte {
	var w proto.Writer
	if v >= 4 {
		writeStatus(&w, status)
	}
	w.U16(rank)
	return w.B
}

// DecodeReaskAck reads OP_REASKACK from a peer of client UDP version v:
// its part status (nil when it has all of the file, or did not say) and
// our place in its queue.
func DecodeReaskAck(payload []byte, v byte) ([]bool, uint16, error) {
	r := proto.NewReader(payload)
	var status []bool
	if v >= 4 {
		var err error
		if status, err = readStatus(r); err != nil {
			return nil, 0, err
		}
	}
	rank := r.U16()
	if r.Err() != nil {
		return nil, 0, errBadUDP
	}
	return status, rank, nil
}
