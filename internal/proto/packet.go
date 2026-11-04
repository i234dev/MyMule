package proto

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxPacketSize protects us against garbage length fields.
const MaxPacketSize = 16 << 20

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

// ReadPacket reads one packet. Packed (0xD4) packets are transparently
// decompressed and returned with Proto set to ProtoEMule.
func ReadPacket(r io.Reader) (*Packet, error) {
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
	if size == 0 || size > MaxPacketSize {
		return nil, fmt.Errorf("proto: invalid packet size %d", size)
	}
	payload := make([]byte, size-1)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	p := &Packet{Proto: proto, Opcode: hdr[5], Payload: payload}
	if proto == ProtoPacked {
		if err := p.unpack(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Packet) unpack() error {
	zr, err := zlib.NewReader(bytes.NewReader(p.Payload))
	if err != nil {
		return fmt.Errorf("proto: zlib: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, MaxPacketSize))
	if err != nil {
		return fmt.Errorf("proto: zlib: %w", err)
	}
	p.Payload = out
	p.Proto = ProtoEMule
	return nil
}
