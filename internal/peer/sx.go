package peer

import (
	"errors"
	"net"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// Source exchange (eMule's): peers that download the same file tell each
// other where else it is. A downloader asks a source with
// OP_REQUESTSOURCES2 <version 1><options 2><file hash>, or with
// OP_REQUESTSOURCES <file hash> (the first form); the answer is
// OP_ANSWERSOURCES2 <version 1><file hash><count 2><sources> or
// OP_ANSWERSOURCES <file hash><count 2><sources>.
//
// A source is <ID 4><TCP port 2><server IP 4><server port 2>, then from
// version 2 its user hash (16), from version 4 its crypt options (1). From
// version 3 the ID is "hybrid": a HighID's IP in the opposite byte order
// to the eD2K ID, so that one whose last byte is 0 is not taken for a
// LowID; a LowID as it is.
const (
	OpAnswerSources   = 0x82 // eMule protocol
	OpRequestSources2 = 0x83
	OpAnswerSources2  = 0x84
)

// SXVersion is the source exchange version we speak (eMule's latest).
const SXVersion = 4

// MaxSXSources is the most sources one answer may carry (eMule stops
// after 500, one more at times); a longer answer is refused whole.
const MaxSXSources = 501

// MaxSXAnswerBytes bounds a source answer: MaxSXSources of the largest
// format, plus the header.
const MaxSXAnswerBytes = 1 + 16 + 2 + MaxSXSources*29

// lowIDLimit: client IDs below it are LowIDs (eD2K's limit).
const lowIDLimit = 0x01000000

// SXSource is one source in a source exchange answer.
type SXSource struct {
	ID         uint32 // eD2K ID: the IP (HighID) or a LowID
	Port       uint16
	ServerIP   net.IP
	ServerPort uint16
	UserHash   ed2k.Hash
	Crypt      byte
}

// SourceExchange1 is the version of the first source exchange form the
// peer speaks (0 = none; eMule asks only peers above 1).
func (h Hello) SourceExchange1() byte { return byte(h.Tags.Int(CTEMuleMiscOpts1)>>12) & 0x0F }

// SourceExchange2 reports whether the peer speaks the second form
// (OP_REQUESTSOURCES2), misc options 2 bit 10.
func (h Hello) SourceExchange2() bool { return h.Tags.Int(CTEMuleMiscOpts2)&(1<<10) != 0 }

// SourceExchange reports whether the peer answers source requests.
func (h Hello) SourceExchange() bool { return h.SourceExchange2() || h.SourceExchange1() > 1 }

func swap32(v uint32) uint32 { return v>>24 | v>>8&0xFF00 | v<<8&0xFF0000 | v<<24 }

func sxEntrySize(version byte) int {
	n := 12
	if version >= 2 {
		n += 16
	}
	if version >= 4 {
		n++
	}
	return n
}

// encodeSources writes count and sources in the given version's format.
// Every ID is taken for a HighID (an IP): we give no LowIDs.
func encodeSources(w *proto.Writer, version byte, srcs []SXSource) {
	w.U16(uint16(len(srcs)))
	for _, s := range srcs {
		id := s.ID
		if version >= 3 {
			id = swap32(id) // every source we give is a HighID
		}
		w.U32(id)
		w.U16(s.Port)
		w.U32(ipToU32(s.ServerIP))
		w.U16(s.ServerPort)
		if version >= 2 {
			w.Bytes(s.UserHash[:])
		}
		if version >= 4 {
			w.U8(s.Crypt)
		}
	}
}

// EncodeAnswerSources2 builds OP_ANSWERSOURCES2.
func EncodeAnswerSources2(version byte, file ed2k.Hash, srcs []SXSource) []byte {
	var w proto.Writer
	w.U8(version)
	w.Bytes(file[:])
	encodeSources(&w, version, srcs)
	return w.B
}

// EncodeAnswerSources builds OP_ANSWERSOURCES in the given version's
// format (the peer's first-form version).
func EncodeAnswerSources(version byte, file ed2k.Hash, srcs []SXSource) []byte {
	var w proto.Writer
	w.Bytes(file[:])
	encodeSources(&w, version, srcs)
	return w.B
}

var errBadSX = errors.New("bad source exchange packet")

// decodeSources reads count and sources, all of them or none.
func decodeSources(r *proto.Reader, version byte) ([]SXSource, error) {
	if version == 0 || version > SXVersion {
		return nil, errBadSX
	}
	n := int(r.U16())
	if r.Err() != nil || n > MaxSXSources || r.Len() != n*sxEntrySize(version) {
		return nil, errBadSX
	}
	srcs := make([]SXSource, 0, n)
	for range n {
		var s SXSource
		s.ID = r.U32()
		if version >= 3 && s.ID >= lowIDLimit {
			s.ID = swap32(s.ID)
		}
		s.Port = r.U16()
		s.ServerIP = ipFromU32(r.U32())
		s.ServerPort = r.U16()
		if version >= 2 {
			s.UserHash = r.Hash()
		}
		if version >= 4 {
			s.Crypt = r.U8()
		}
		srcs = append(srcs, s)
	}
	return srcs, r.Err()
}

// DecodeAnswerSources2 parses OP_ANSWERSOURCES2.
func DecodeAnswerSources2(payload []byte) (byte, ed2k.Hash, []SXSource, error) {
	r := proto.NewReader(payload)
	version := r.U8()
	file := ed2k.Hash(r.Hash())
	if r.Err() != nil {
		return 0, file, nil, errBadSX
	}
	srcs, err := decodeSources(r, version)
	return version, file, srcs, err
}

// DecodeAnswerSources parses OP_ANSWERSOURCES. It says no version, and
// clients write it in theirs or ours: the size of the sources tells (as
// eMule reads it), and version (the one we asked with) only chooses
// between 2 and 3, which have the same size.
func DecodeAnswerSources(payload []byte, version byte) (ed2k.Hash, []SXSource, error) {
	r := proto.NewReader(payload)
	file := ed2k.Hash(r.Hash())
	n := int(proto.NewReader(payload[min(len(payload), 16):]).U16())
	if r.Err() != nil || len(payload) < 18 {
		return file, nil, errBadSX
	}
	if rest := len(payload) - 18; n > 0 && rest%n == 0 {
		switch rest / n {
		case sxEntrySize(1):
			version = 1
		case sxEntrySize(2):
			version = min(max(version, 2), 3)
		case sxEntrySize(4):
			version = 4
		}
	}
	srcs, err := decodeSources(r, version)
	return file, srcs, err
}

// EncodeRequestSources2 builds OP_REQUESTSOURCES2 (no options).
func EncodeRequestSources2(file ed2k.Hash) []byte {
	var w proto.Writer
	w.U8(SXVersion)
	w.U16(0)
	w.Bytes(file[:])
	return w.B
}

// DecodeRequestSources reads a source request of either form: the file
// and the version to answer with (for the first form, the peer's
// first-form version, which the caller passes as v1).
func DecodeRequestSources(p *proto.Packet, v1 byte) (ed2k.Hash, byte, bool) {
	r := proto.NewReader(p.Payload)
	version := v1
	if p.Opcode == OpRequestSources2 {
		version = r.U8()
		r.U16() // options: none we act on
	}
	file := ed2k.Hash(r.Hash())
	if r.Err() != nil || version == 0 {
		return file, 0, false
	}
	return file, min(version, SXVersion), true
}
