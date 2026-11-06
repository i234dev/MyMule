package proto

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxPacketSize is the largest packet accepted from a server, before or
// after decompression. Real server messages are far smaller.
const MaxPacketSize = 4 << 20

// MaxPeerPacketSize is the largest packet accepted from another client.
// Data comes in 10 KB packets and the biggest legitimate message, the
// part hash list of a 256 GB file, is under 500 KB.
const MaxPeerPacketSize = 1 << 20

// Packet is a single eD2K TCP packet.
//
// Wire layout: [proto u8][size u32 LE][opcode u8][payload ...]
// where size counts the opcode byte plus the payload.
type Packet struct {
	Proto   byte
	Opcode  byte
	Payload []byte
}

// Encode serialises the packet for the wire.
func (p *Packet) Encode() []byte {
	b := make([]byte, 6+len(p.Payload))
	b[0] = p.Proto
	binary.LittleEndian.PutUint32(b[1:], uint32(len(p.Payload)+1))
	b[5] = p.Opcode
	copy(b[6:], p.Payload)
	return b
}

// ReadPacket reads one packet of at most MaxPacketSize bytes. Packed (0xD4)
// packets are transparently decompressed and returned with Proto set to
// ProtoEMule.
func ReadPacket(r io.Reader) (*Packet, error) { return ReadPacketMax(r, MaxPacketSize) }

// ReadPacketMax is ReadPacket with a size limit that applies both to the
// packet on the wire and to its decompressed payload.
func ReadPacketMax(r io.Reader, max int) (*Packet, error) {
	var hdr [6]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	proto := hdr[0]
	switch proto {
	case ProtoEDonkey, ProtoEMule, ProtoPacked:
	default:
		return nil, fmt.Errorf("proto: unknown protocol byte 0x%02X", proto)
	}
	size := binary.LittleEndian.Uint32(hdr[1:5])
	if size == 0 || int64(size) > int64(max) {
		return nil, fmt.Errorf("proto: invalid packet size %d", size)
	}
	payload, err := readN(r, int(size-1))
	if err != nil {
		return nil, err
	}
	p := &Packet{Proto: proto, Opcode: hdr[5], Payload: payload}
	if proto == ProtoPacked {
		if err := p.unpack(max); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// readChunk is how much readN allocates ahead of the data it has received.
const readChunk = 64 << 10

// readN reads exactly n bytes. The buffer grows as data arrives, so a
// header that announces a large packet and then sends little or nothing
// costs only the memory for what was actually received.
func readN(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, 0, min(n, readChunk))
	for len(buf) < n {
		if len(buf) == cap(buf) {
			grown := make([]byte, len(buf), min(n, max(2*cap(buf), readChunk)))
			copy(grown, buf)
			buf = grown
		}
		k, err := io.ReadFull(r, buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+k]
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// unpack decompresses a packed payload. A few KB of zlib can expand to
// many MB, so anything that grows beyond max is rejected outright.
func (p *Packet) unpack(max int) error {
	zr, err := zlib.NewReader(bytes.NewReader(p.Payload))
	if err != nil {
		return fmt.Errorf("proto: zlib: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, int64(max)+1))
	if err != nil {
		return fmt.Errorf("proto: zlib: %w", err)
	}
	if len(out) > max {
		return fmt.Errorf("proto: packed packet expands beyond %d bytes", max)
	}
	p.Payload = out
	p.Proto = ProtoEMule
	return nil
}
