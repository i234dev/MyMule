// Package peer implements the eD2K client <-> client wire protocol:
// opcodes, the hello handshake and a framed connection.
package peer

import (
	"fmt"
	"net"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/netutil"
	"mymule/internal/proto"
)

// Client <-> client opcodes on the eDonkey protocol (0xE3).
const (
	OpHello            = 0x01
	OpSendingPart      = 0x46
	OpRequestParts     = 0x47
	OpFileReqAnsNoFil  = 0x48
	OpEndOfDownload    = 0x49
	OpAskSharedFiles   = 0x4A
	OpHelloAnswer      = 0x4C
	OpMessage          = 0x4E
	OpSetReqFileID     = 0x4F
	OpFileStatus       = 0x50
	OpHashSetRequest   = 0x51
	OpHashSetAnswer    = 0x52
	OpStartUploadReq   = 0x54
	OpAcceptUploadReq  = 0x55
	OpCancelTransfer   = 0x56
	OpOutOfPartReqs    = 0x57
	OpRequestFileName  = 0x58
	OpReqFileNameAns   = 0x59
	OpQueueRank        = 0x5C
	OpAskSharedDirs    = 0x5D
	OpAskSharedDirsAns = 0x5F
)

// Client <-> client opcodes on the eMule extended protocol (0xC5).
const (
	OpEMuleInfo         = 0x01
	OpEMuleInfoAnswer   = 0x02
	OpCompressedPart    = 0x40
	OpQueueRanking      = 0x60
	OpRequestSources    = 0x81
	OpPublicKey         = 0x85 // secure identification: <len 1><key>
	OpSignature         = 0x86 // <len 1><signature>[<IP kind 1>]
	OpSecIdentState     = 0x87 // <what we need 1><challenge 4>
	OpCompressedPartI64 = 0xA1
	OpSendingPartI64    = 0xA2
	OpRequestPartsI64   = 0xA3
	OpKadFWTCPCheckAck  = 0xA8 // Kad: "I connected to you" (firewall check)
)

// Hello tag names.
const (
	CTName           = 0x01
	CTVersion        = 0x11
	CTEMuleUDPPorts  = 0xF9
	CTEMuleMiscOpts1 = 0xFA
	CTEMuleVersion   = 0xFB
	CTEMuleMiscOpts2 = 0xFE
)

// BlockSize is the unit in which file data is requested (180 KiB).
const BlockSize = 184320

// LargeFileLimit: files above this need the 64-bit packet variants.
const LargeFileLimit = 4290048000

const (
	edonkeyVersion = 0x3C
	emuleVersion   = 0<<17 | 50<<10 | 0<<7

	// Misc options 1: we support AICH (bits 29-31), unicode, compressed
	// transfers (bits 20-23), secure identification (both versions, bits
	// 16-19), source exchange version 4 (bits 12-15) and do not let others
	// browse our shared files. Everything else (extended requests...) is
	// 0 = unsupported, so peers stick to the protocol subset we implement.
	miscOpts1 = AICHVersion<<29 | 1<<28 | DataCompVersion<<20 | 3<<16 | SXVersion<<12 | 1<<2
	// Misc options 2: large file (>4 GB) support and the second form of
	// source exchange (bit 10).
	miscOpts2 = 1<<4 | 1<<10
)

// Hello is the content of OP_HELLO / OP_HELLOANSWER.
type Hello struct {
	UserHash   ed2k.Hash
	ClientID   uint32
	Port       uint16
	Tags       proto.TagList
	ServerIP   net.IP
	ServerPort uint16
}

// Name is the peer's nick, made safe to print (it comes from the peer).
func (h Hello) Name() string { return netutil.CleanText(h.Tags.Str(CTName), 64) }

// KadVersion is the Kad version the peer says it speaks (0 = none).
func (h Hello) KadVersion() byte { return byte(h.Tags.Int(CTEMuleMiscOpts2) & 0x0F) }

// SecIdent is the secure identification the peer supports: bit 0
// version 1, bit 1 version 2 (0 = none).
func (h Hello) SecIdent() byte { return byte(h.Tags.Int(CTEMuleMiscOpts1)>>16) & 0x0F }

// Crypt is the obfuscation the peer offers in its hello (misc options 2,
// bits 7-9), as crypt options: 1 supports, 2 requests, 4 requires.
func (h Hello) Crypt() byte { return byte(h.Tags.Int(CTEMuleMiscOpts2)>>7) & 0x07 }

// WithCrypt sets the obfuscation a hello we send offers (crypt options,
// as Crypt returns them).
func (h Hello) WithCrypt(opts byte) Hello {
	tags := make(proto.TagList, 0, len(h.Tags))
	for _, t := range h.Tags {
		if t.Name == "" && t.ID == CTEMuleMiscOpts2 {
			t.Int = t.Int&^(7<<7) | uint64(opts&7)<<7
		}
		tags = append(tags, t)
	}
	h.Tags = tags
	return h
}

// KadPort is the peer's Kad UDP port (0 = none).
func (h Hello) KadPort() uint16 { return uint16(h.Tags.Int(CTEMuleUDPPorts) >> 16) }

// WithKad adds our Kad UDP port and Kad version to a hello we send, so
// peers know we run Kad (and answer Kad firewall checks over TCP).
func (h Hello) WithKad(port uint16, version byte) Hello {
	tags := make(proto.TagList, 0, len(h.Tags)+1)
	for _, t := range h.Tags {
		if t.Name == "" && t.ID == CTEMuleMiscOpts2 {
			t.Int = t.Int&^0x0F | uint64(version&0x0F)
		}
		tags = append(tags, t)
	}
	// High 16 bits: Kad port; low: the eD2K client UDP port, which we
	// do not have.
	h.Tags = append(tags, proto.IntTag(CTEMuleUDPPorts, uint32(port)<<16))
	return h
}

// WithUDPPort sets the eD2K UDP port our hello gives (the low 16 bits of
// the UDP ports tag; the Kad port stays).
func (h Hello) WithUDPPort(port uint16) Hello {
	tags := make(proto.TagList, 0, len(h.Tags))
	for _, t := range h.Tags {
		if t.Name == "" && t.ID == CTEMuleUDPPorts {
			t.Int = t.Int&^0xFFFF | uint64(port)
		}
		tags = append(tags, t)
	}
	h.Tags = tags
	return h
}

// Hello tags of a firewalled Kad client: its buddy's IP (network order)
// and UDP port.
const (
	CTEMuleBuddyIP  = 0xFC
	CTEMuleBuddyUDP = 0xFD
)

// WithKadFirewall adds to a hello we send our Kad buddy (when we have
// one) and whether we take direct callbacks (misc options 2, bit 12).
func (h Hello) WithKadFirewall(buddyIP net.IP, buddyPort uint16, direct bool) Hello {
	tags := make(proto.TagList, 0, len(h.Tags)+2)
	for _, t := range h.Tags {
		if t.Name == "" && t.ID == CTEMuleMiscOpts2 {
			t.Int &^= 1 << 12
			if direct {
				t.Int |= 1 << 12
			}
		}
		tags = append(tags, t)
	}
	if buddyIP.To4() != nil && buddyPort != 0 {
		tags = append(tags, proto.IntTag(CTEMuleBuddyIP, ipToU32(buddyIP)), proto.IntTag(CTEMuleBuddyUDP, uint32(buddyPort)))
	}
	h.Tags = tags
	return h
}

// NewHello builds the hello we send to peers.
func NewHello(userHash ed2k.Hash, nick string, clientID uint32, port uint16, srvIP net.IP, srvPort uint16) Hello {
	return Hello{
		UserHash: userHash,
		ClientID: clientID,
		Port:     port,
		Tags: proto.TagList{
			proto.StrTag(CTName, nick),
			proto.IntTag(CTVersion, edonkeyVersion),
			proto.IntTag(CTEMuleMiscOpts1, miscOpts1),
			proto.IntTag(CTEMuleMiscOpts2, miscOpts2),
			proto.IntTag(CTEMuleVersion, emuleVersion),
		},
		ServerIP:   srvIP,
		ServerPort: srvPort,
	}
}

// Encode serialises a hello. OP_HELLO carries a leading hash-size byte,
// OP_HELLOANSWER does not.
func (h Hello) Encode(answer bool) []byte {
	var w proto.Writer
	if !answer {
		w.U8(16)
	}
	w.Bytes(h.UserHash[:])
	w.U32(h.ClientID)
	w.U16(h.Port)
	w.U32(uint32(len(h.Tags)))
	for _, t := range h.Tags {
		w.WriteTag(t)
	}
	w.U32(ipToU32(h.ServerIP))
	w.U16(h.ServerPort)
	return w.B
}

// DecodeHello parses OP_HELLO (answer=false) or OP_HELLOANSWER (answer=true).
func DecodeHello(payload []byte, answer bool) (Hello, error) {
	var h Hello
	r := proto.NewReader(payload)
	if !answer {
		if n := r.U8(); n != 16 {
			return h, fmt.Errorf("hello: unexpected hash size %d", n)
		}
	}
	h.UserHash = r.Hash()
	h.ClientID = r.U32()
	h.Port = r.U16()
	tags, err := r.ReadTags(int(r.U32()))
	if err != nil {
		return h, fmt.Errorf("hello: %w", err)
	}
	h.Tags = tags
	if r.Len() >= 6 {
		h.ServerIP = ipFromU32(r.U32())
		h.ServerPort = r.U16()
	}
	return h, r.Err()
}

func ipFromU32(v uint32) net.IP {
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).To4()
}

func ipToU32(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return uint32(ip4[0]) | uint32(ip4[1])<<8 | uint32(ip4[2])<<16 | uint32(ip4[3])<<24
}

// Conn is a framed eD2K peer connection, safe for concurrent writes.
type Conn struct {
	nc      net.Conn
	writeMu sync.Mutex
	// Trace, if set, is called for every packet sent (out=true) or received.
	Trace func(out bool, p *proto.Packet)
	// Packed, if set, says which compressed packets Read takes and how
	// large they may grow (0 = refused, as all are by default). Set and
	// read by the reading goroutine only.
	Packed func(opcode byte) int
}

func NewConn(nc net.Conn) *Conn { return &Conn{nc: nc} }

func (c *Conn) RemoteIP() net.IP {
	if a, ok := c.nc.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP.To4()
	}
	return nil
}

// LocalIP is our address on this connection (IPv4), or nil.
func (c *Conn) LocalIP() net.IP {
	if a, ok := c.nc.LocalAddr().(*net.TCPAddr); ok {
		return a.IP.To4()
	}
	return nil
}

func (c *Conn) Close() error { return c.nc.Close() }

// Send writes a packet with a 30 s write deadline.
func (c *Conn) Send(protocol, opcode byte, payload []byte) error {
	p := proto.Packet{Proto: protocol, Opcode: opcode, Payload: payload}
	if c.Trace != nil {
		c.Trace(true, &p)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.nc.Write(p.Encode())
	return err
}

// Read reads the next packet, failing if none arrives within timeout.
func (c *Conn) Read(timeout time.Duration) (*proto.Packet, error) {
	c.nc.SetReadDeadline(time.Now().Add(timeout))
	p, err := proto.ReadPeerPacketPacked(c.nc, proto.MaxPeerPacketSize, c.Packed)
	if err == nil && c.Trace != nil {
		c.Trace(false, p)
	}
	return p, err
}

// SendHash sends a packet whose payload is only a file hash.
func (c *Conn) SendHash(protocol, opcode byte, h ed2k.Hash) error {
	return c.Send(protocol, opcode, h[:])
}

// RequestParts asks for up to three byte ranges [start, end) of a file.
// Unused slots are sent as 0/0, which peers ignore.
func (c *Conn) RequestParts(h ed2k.Hash, large bool, ranges [][2]int64) error {
	var starts, ends [3]int64
	for i := 0; i < 3 && i < len(ranges); i++ {
		starts[i], ends[i] = ranges[i][0], ranges[i][1]
	}
	var w proto.Writer
	w.Bytes(h[:])
	if large {
		for _, v := range starts {
			w.U64(uint64(v))
		}
		for _, v := range ends {
			w.U64(uint64(v))
		}
		return c.Send(proto.ProtoEMule, OpRequestPartsI64, w.B)
	}
	for _, v := range starts {
		w.U32(uint32(v))
	}
	for _, v := range ends {
		w.U32(uint32(v))
	}
	return c.Send(proto.ProtoEDonkey, OpRequestParts, w.B)
}

// DecodeFileStatus parses OP_FILESTATUS. A nil slice means the peer has
// the complete file.
func DecodeFileStatus(payload []byte) (ed2k.Hash, []bool, error) {
	r := proto.NewReader(payload)
	h := ed2k.Hash(r.Hash())
	n := int(r.U16())
	if n == 0 {
		return h, nil, r.Err()
	}
	bits := r.Bytes((n + 7) / 8)
	if r.Err() != nil {
		return h, nil, r.Err()
	}
	avail := make([]bool, n)
	for i := range avail {
		avail[i] = bits[i/8]&(1<<(i%8)) != 0
	}
	return h, avail, nil
}

// DecodeHashSet parses OP_HASHSETANSWER.
func DecodeHashSet(payload []byte) (ed2k.Hash, []ed2k.Hash, error) {
	r := proto.NewReader(payload)
	h := ed2k.Hash(r.Hash())
	n := int(r.U16())
	set := make([]ed2k.Hash, 0, n)
	for i := 0; i < n && r.Err() == nil; i++ {
		set = append(set, r.Hash())
	}
	return h, set, r.Err()
}

// DataPart is a decoded OP_SENDINGPART / OP_COMPRESSEDPART packet.
type DataPart struct {
	Hash       ed2k.Hash
	Start      int64
	End        int64 // exclusive; for compressed parts: 0
	PackedSize uint32
	Compressed bool
	Data       []byte
}

// DecodeDataPart decodes the four data packet variants.
func DecodeDataPart(p *proto.Packet) (DataPart, error) {
	var d DataPart
	r := proto.NewReader(p.Payload)
	d.Hash = r.Hash()
	switch {
	case p.Proto == proto.ProtoEDonkey && p.Opcode == OpSendingPart:
		d.Start, d.End = int64(r.U32()), int64(r.U32())
	case p.Proto == proto.ProtoEMule && p.Opcode == OpSendingPartI64:
		d.Start, d.End = int64(r.U64()), int64(r.U64())
	case p.Proto == proto.ProtoEMule && p.Opcode == OpCompressedPart:
		d.Start, d.PackedSize, d.Compressed = int64(r.U32()), r.U32(), true
	case p.Proto == proto.ProtoEMule && p.Opcode == OpCompressedPartI64:
		d.Start, d.PackedSize, d.Compressed = int64(r.U64()), r.U32(), true
	default:
		return d, fmt.Errorf("not a data packet: 0x%02X/0x%02X", p.Proto, p.Opcode)
	}
	if r.Err() != nil {
		return d, r.Err()
	}
	d.Data = r.B
	// 64-bit offsets above 2^63 turn negative as int64: refuse them here
	// rather than rely on every later check.
	// An empty packet carries nothing: refuse it as eMule and aMule do
	// (accepting it let a peer look busy while sending no data).
	if d.Start < 0 || (!d.Compressed && d.End <= d.Start) {
		return d, fmt.Errorf("data packet with an impossible range %d-%d", d.Start, d.End)
	}
	if !d.Compressed && d.End-d.Start != int64(len(d.Data)) {
		return d, fmt.Errorf("data packet size mismatch: range %d-%d, %d bytes", d.Start, d.End, len(d.Data))
	}
	return d, nil
}

// IsDataPacket reports whether p carries file data.
func IsDataPacket(p *proto.Packet) bool {
	switch {
	case p.Proto == proto.ProtoEDonkey && p.Opcode == OpSendingPart:
		return true
	case p.Proto == proto.ProtoEMule && (p.Opcode == OpSendingPartI64 ||
		p.Opcode == OpCompressedPart || p.Opcode == OpCompressedPartI64):
		return true
	}
	return false
}

// EMuleInfoAnswer is our reply to OP_EMULEINFO from old clients.
func EMuleInfoAnswer() []byte {
	var w proto.Writer
	w.U8(0x50) // client version
	w.U8(0x01) // protocol version
	w.U32(0)   // no tags: no optional features
	return w.B
}
