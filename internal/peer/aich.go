package peer

import (
	"errors"

	"mymule/internal/aich"
	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// AICH (eMule extended protocol):
//
//	OP_AICHFILEHASHREQ <file hash 16>
//	OP_AICHFILEHASHANS <file hash 16><AICH root 20>
//	OP_AICHREQUEST     <file hash 16><part 2><AICH root 20>
//	OP_AICHANSWER      <file hash 16><part 2><AICH root 20><recovery data>
//	                   or <file hash 16> alone: none to give
const (
	OpAICHRequest     = 0x9B
	OpAICHAnswer      = 0x9C
	OpAICHFileHashAns = 0x9D
	OpAICHFileHashReq = 0x9E
)

// AICHVersion is the AICH version we speak (misc options 1, bits 29-31).
const AICHVersion = 1

// AICH is the peer's AICH version (0 = none).
func (h Hello) AICH() byte { return byte(h.Tags.Int(CTEMuleMiscOpts1)>>29) & 0x07 }

var errAICHPacket = errors.New("malformed AICH packet")

// EncodeAICHFileHash is OP_AICHFILEHASHANS.
func EncodeAICHFileHash(file ed2k.Hash, root aich.Hash) []byte {
	return append(append([]byte(nil), file[:]...), root[:]...)
}

// DecodeAICHFileHash reads OP_AICHFILEHASHANS.
func DecodeAICHFileHash(p []byte) (ed2k.Hash, aich.Hash, error) {
	var f ed2k.Hash
	var r aich.Hash
	if len(p) < 36 {
		return f, r, errAICHPacket
	}
	copy(f[:], p)
	copy(r[:], p[16:])
	return f, r, nil
}

// EncodeAICHRequest is OP_AICHREQUEST.
func EncodeAICHRequest(file ed2k.Hash, part int, root aich.Hash) []byte {
	var w proto.Writer
	w.Bytes(file[:])
	w.U16(uint16(part))
	w.Bytes(root[:])
	return w.B
}

// DecodeAICHRequest reads OP_AICHREQUEST.
func DecodeAICHRequest(p []byte) (ed2k.Hash, int, aich.Hash, error) {
	var f ed2k.Hash
	var r aich.Hash
	if len(p) < 38 {
		return f, 0, r, errAICHPacket
	}
	rd := proto.NewReader(p)
	copy(f[:], rd.Bytes(16))
	part := int(rd.U16())
	copy(r[:], rd.Bytes(20))
	return f, part, r, nil
}

// EncodeAICHAnswer is OP_AICHANSWER; data nil: no recovery data.
func EncodeAICHAnswer(file ed2k.Hash, part int, root aich.Hash, data []byte) []byte {
	if data == nil {
		return append([]byte(nil), file[:]...)
	}
	return append(EncodeAICHRequest(file, part, root), data...)
}

// DecodeAICHAnswer reads OP_AICHANSWER; data is nil when the peer had no
// recovery data to give.
func DecodeAICHAnswer(p []byte) (file ed2k.Hash, part int, root aich.Hash, data []byte, err error) {
	if len(p) < 16 {
		return file, 0, root, nil, errAICHPacket
	}
	copy(file[:], p)
	if len(p) == 16 {
		return file, 0, root, nil, nil
	}
	file, part, root, err = DecodeAICHRequest(p)
	if err != nil {
		return file, 0, root, nil, err
	}
	return file, part, root, p[38:], nil
}
