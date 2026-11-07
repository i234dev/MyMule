package kad

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// Protocol bytes of UDP datagrams on the Kad port.
const (
	ProtoKad       = 0xE4 // Kad packet
	ProtoKadPacked = 0xE5 // Kad packet with a zlib-compressed payload
	ProtoEMule     = 0xC5 // eMule client-to-client UDP (callbacks...)
)

// Protocol bytes an obfuscated datagram never starts with (eMule's
// EncryptedDatagramSocket): a datagram starting with one of these is
// plain.
var plainMarkers = [...]byte{0xC5, 0xE5, 0xE4, 0xA3, 0xB2, 0xD4}

func isPlainMarker(b byte) bool {
	for _, m := range plainMarkers {
		if b == m {
			return true
		}
	}
	return false
}

// Kad version 2 opcodes (the only ones current clients use).
const (
	OpBootstrapReq     = 0x01 // (empty)
	OpBootstrapRes     = 0x09 // <ID><u16 tcp><u8 ver><u16 n>(<ID><u32 ip><u16 udp><u16 tcp><u8 ver>)*n
	OpHelloReq         = 0x11 // <ID><u16 tcp><u8 ver><u8 ntags><tags>
	OpHelloRes         = 0x19 // same as OpHelloReq
	OpReq              = 0x21 // <u8 type><ID target><ID receiver>
	OpHelloResAck      = 0x22 // <ID><u8 ntags>
	OpRes              = 0x29 // <ID target><u8 n>(<ID><u32 ip><u16 udp><u16 tcp><u8 ver>)*n
	OpSearchKeyReq     = 0x33 // <ID><u16 start|0x8000 if restrictive>[search tree]
	OpSearchSourceReq  = 0x34 // <ID><u16 start><u64 size>
	OpSearchNotesReq   = 0x35 // <ID><u64 size>
	OpSearchRes        = 0x3B // <ID sender><ID target><u16 n>(<ID answer><tags>)*n
	OpPublishKeyReq    = 0x43 // <ID keyword><u16 n>(<ID file><tags>)*n
	OpPublishSourceReq = 0x44 // <ID file><ID source><tags>
	OpPublishNotesReq  = 0x45 // <ID file><ID source><tags>
	OpPublishRes       = 0x4B // <ID target><u8 load>[<u8 options: 1 = wants an ack>]
	OpPublishResAck    = 0x4C // (empty)
	OpFirewalled2Req   = 0x53 // <u16 tcp><user hash><u8 connect options>
	OpPing             = 0x60 // (empty)
	OpPong             = 0x61 // <u16 the UDP port we were seen from>
	OpFirewallUDP      = 0x62 // <u8 error><u16 port used>
)

// Kad version 1 opcodes still in use.
const (
	OpSearchResV1      = 0x38 // <ID target><u16 n>(<ID answer><tags>)*n
	OpSearchNotesResV1 = 0x3A
	OpPublishResV1     = 0x48 // <ID target>
	OpFirewalledReq    = 0x50 // <u16 tcp>
	OpFindBuddyReq     = 0x51 // <ID buddy><ID user hash><u16 tcp>
	OpCallbackReq      = 0x52 // <ID buddy><ID file><u16 tcp>
	OpFirewalledRes    = 0x58 // <u32 ip> (the address we were seen from)
	OpFirewalledAckRes = 0x59 // (empty)
	OpFindBuddyRes     = 0x5A // <ID buddy><ID user hash><u16 tcp>[<u8 connect options>]
)

// Version is the Kad version we speak: eMule 0.49b's, the last one with
// protocol changes.
const Version = 0x08

// Packet is one Kad datagram, before obfuscation.
type Packet struct {
	Opcode  byte
	Payload []byte
}

// packThreshold is the payload size from which eMule compresses a packet.
const packThreshold = 200

// Encode returns the datagram: [0xE4][opcode][payload], or [0xE5][opcode]
// [zlib payload] when the payload is over 200 bytes and compressing makes
// it smaller (but not so much smaller that eMule would refuse to unpack
// it, see unpackLimit).
func (p Packet) Encode() []byte {
	if len(p.Payload) > packThreshold {
		var buf bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
		zw.Write(p.Payload)
		if zw.Close() == nil && buf.Len() < len(p.Payload) && len(p.Payload) <= unpackLimit(2+buf.Len()) {
			return append([]byte{ProtoKadPacked, p.Opcode}, buf.Bytes()...)
		}
	}
	return append([]byte{ProtoKad, p.Opcode}, p.Payload...)
}

// unpackLimit is the most the payload of a compressed datagram of n bytes
// may expand to: eMule's limit, 10 times the datagram plus 300 bytes, less
// the 2-byte header. A datagram is at most 64 KB, so this is under 1 MB.
func unpackLimit(n int) int { return n*10 + 300 - 2 }

// ErrNotKad is returned by DecodePacket for a datagram of another protocol.
var ErrNotKad = errors.New("kad: not a Kad packet")

// DecodePacket parses a plain (or already deobfuscated) datagram.
func DecodePacket(b []byte) (Packet, error) {
	if len(b) < 2 {
		return Packet{}, errors.New("kad: packet too short")
	}
	switch b[0] {
	case ProtoKad:
		return Packet{Opcode: b[1], Payload: append([]byte(nil), b[2:]...)}, nil
	case ProtoKadPacked:
		limit := unpackLimit(len(b))
		zr, err := zlib.NewReader(bytes.NewReader(b[2:]))
		if err != nil {
			return Packet{}, fmt.Errorf("kad: zlib: %w", err)
		}
		defer zr.Close()
		out, err := io.ReadAll(io.LimitReader(zr, int64(limit)+1))
		if err != nil {
			return Packet{}, fmt.Errorf("kad: zlib: %w", err)
		}
		if len(out) > limit {
			return Packet{}, errors.New("kad: compressed packet expands too much")
		}
		return Packet{Opcode: b[1], Payload: out}, nil
	}
	return Packet{}, ErrNotKad
}
