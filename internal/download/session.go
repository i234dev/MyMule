package download

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/proto"
)

const (
	handshakeTimeout = 30 * time.Second
	askTimeout       = 40 * time.Second
	queuedTimeout    = 15 * time.Minute // stay connected while the peer allows it
	transferTimeout  = 100 * time.Second
	pipelineBlocks   = 3
)

var errDone = errors.New("session finished")

// runSession performs the hello handshake on a fresh connection, works out
// which source it belongs to and then drives the download protocol.
func (m *Manager) runSession(ctx context.Context, nc net.Conn, outbound bool, src *Source) {
	c := peer.NewConn(nc)
	if m.cfg.Trace {
		remote := nc.RemoteAddr().String()
		c.Trace = func(out bool, p *proto.Packet) {
			dir := "<-"
			if out {
				dir = "->"
			}
			n := len(p.Payload)
			if peer.IsDataPacket(p) {
				m.cfg.Debug("trace %s %s 0x%02X/0x%02X (%d bytes data)", remote, dir, p.Proto, p.Opcode, n)
				return
			}
			show := p.Payload
			if len(show) > 48 {
				show = show[:48]
			}
			m.cfg.Debug("trace %s %s 0x%02X/0x%02X len=%d % x", remote, dir, p.Proto, p.Opcode, n, show)
		}
	}
	m.trackConn(c, true)
	defer m.trackConn(c, false)
	defer c.Close()

	hello, err := m.handshake(c, outbound)
	if err != nil {
		m.cfg.Debug("[%s]: handshake failed: %v", nc.RemoteAddr(), err)
		if src != nil {
			m.mu.Lock()
			src.Fails++
			src.State = StateUnreachable
			src.NextAsk = time.Now().Add(time.Duration(src.Fails) * 5 * time.Minute)
			m.mu.Unlock()
			m.releaseSource(src)
		}
		return
	}

	if src == nil {
		src = m.claimSource(hello, c.RemoteIP())
		if src == nil {
			// Someone who wants something from us; we don't share yet.
			serveNothing(c)
			return
		}
	}
	defer m.releaseSource(src)

	m.bindConn(c, src.file)
	m.mu.Lock()
	src.UserHash = hello.UserHash
	src.Name = hello.Name()
	src.Fails = 0
	f := src.file
	m.mu.Unlock()

	s := &session{m: m, c: c, src: src, f: f, pf: f.pf, owner: src.key, packed: map[int64]*packedBlock{}}
	err = s.run(ctx)
	s.releaseAll()

	m.mu.Lock()
	switch src.State {
	case StateNoFile:
		// never asked again
	case StateQueued, StateAsking, StateConnecting:
		// Reask relative to our last upload request, as eMule expects.
		src.State = StateQueued
		src.NextAsk = s.askedAt.Add(ReaskInterval)
		if s.askedAt.IsZero() {
			src.NextAsk = time.Now().Add(ReaskInterval)
		}
	default:
		src.NextAsk = time.Now().Add(ReaskInterval)
		if src.State == StateDownloading {
			src.State = StateQueued
		}
	}
	m.mu.Unlock()

	if ctx.Err() == nil {
		m.cfg.Debug("%s [%s %q]: session ended (%s, rank %d): %v", f.pf.Name, src.key, src.Name, src.State, src.Rank, err)
	}
}

// handshake exchanges OP_HELLO / OP_HELLOANSWER.
func (m *Manager) handshake(c *peer.Conn, outbound bool) (peer.Hello, error) {
	ours := m.ourHello()
	if outbound {
		if err := c.Send(proto.ProtoEDonkey, peer.OpHello, ours.Encode(false)); err != nil {
			return peer.Hello{}, err
		}
	}
	for i := 0; i < 4; i++ {
		p, err := c.Read(handshakeTimeout)
		if err != nil {
			return peer.Hello{}, err
		}
		switch {
		case p.Proto == proto.ProtoEDonkey && outbound && p.Opcode == peer.OpHelloAnswer:
			return peer.DecodeHello(p.Payload, true)
		case p.Proto == proto.ProtoEDonkey && !outbound && p.Opcode == peer.OpHello:
			h, err := peer.DecodeHello(p.Payload, false)
			if err != nil {
				return h, err
			}
			return h, c.Send(proto.ProtoEDonkey, peer.OpHelloAnswer, ours.Encode(true))
		case p.Proto == proto.ProtoEMule && p.Opcode == peer.OpEMuleInfo:
			c.Send(proto.ProtoEMule, peer.OpEMuleInfoAnswer, peer.EMuleInfoAnswer())
		}
	}
	return peer.Hello{}, errors.New("no hello received")
}

// serveNothing politely answers file requests from a peer we have nothing
// for, then closes.
func serveNothing(c *peer.Conn) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p, err := c.Read(10 * time.Second)
		if err != nil {
			return
		}
		if p.Proto == proto.ProtoEDonkey && (p.Opcode == peer.OpRequestFileName || p.Opcode == peer.OpSetReqFileID) && len(p.Payload) >= 16 {
			c.Send(proto.ProtoEDonkey, peer.OpFileReqAnsNoFil, p.Payload[:16])
		}
	}
}

type packedBlock struct {
	size uint32
	buf  bytes.Buffer
}

type session struct {
	m     *Manager
	c     *peer.Conn
	src   *Source
	f     *File
	pf    *PartFile
	owner string

	pending []Range // blocks requested from this peer
	packed  map[int64]*packedBlock
	askedAt time.Time
	timeout time.Duration
}

func (s *session) setState(st SourceState) {
	s.m.mu.Lock()
	s.src.State = st
	s.m.mu.Unlock()
}

func (s *session) avail() []bool {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	return s.src.Avail
}

func (s *session) run(ctx context.Context) error {
	h := s.pf.Hash
	s.setState(StateAsking)
	if err := s.c.SendHash(proto.ProtoEDonkey, peer.OpRequestFileName, h); err != nil {
		return err
	}
	if err := s.c.SendHash(proto.ProtoEDonkey, peer.OpSetReqFileID, h); err != nil {
		return err
	}
	s.timeout = askTimeout

	for ctx.Err() == nil {
		p, err := s.c.Read(s.timeout)
		if err != nil {
			return err
		}
		if err := s.handle(p); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *session) handle(p *proto.Packet) error {
	if peer.IsDataPacket(p) {
		return s.onData(p)
	}
	r := proto.NewReader(p.Payload)
	if p.Proto == proto.ProtoEMule {
		switch p.Opcode {
		case peer.OpEMuleInfo:
			return s.c.Send(proto.ProtoEMule, peer.OpEMuleInfoAnswer, peer.EMuleInfoAnswer())
		case peer.OpQueueRanking:
			s.onQueued(int(r.U16()))
		}
		return nil
	}
	if p.Proto != proto.ProtoEDonkey {
		return nil
	}

	switch p.Opcode {
	case peer.OpReqFileNameAns, peer.OpEndOfDownload:
		// informational

	case peer.OpFileReqAnsNoFil:
		if ed2k.Hash(r.Hash()) == s.pf.Hash {
			s.setState(StateNoFile)
			return errDone
		}

	case peer.OpFileStatus:
		h, avail, err := peer.DecodeFileStatus(p.Payload)
		if err != nil || h != s.pf.Hash {
			return err
		}
		if avail != nil && len(avail) != NumParts(s.pf.Size) {
			avail = nil // malformed; assume complete like eMule does for count 0
		}
		s.m.mu.Lock()
		s.src.Avail = avail
		s.m.mu.Unlock()
		if !s.pf.HasNeededParts(avail) {
			s.setState(StateNoNeededParts)
			return errDone
		}
		if s.pf.NeedsHashSet() {
			if err := s.c.SendHash(proto.ProtoEDonkey, peer.OpHashSetRequest, s.pf.Hash); err != nil {
				return err
			}
		}
		if s.askedAt.IsZero() {
			s.askedAt = time.Now()
			return s.c.SendHash(proto.ProtoEDonkey, peer.OpStartUploadReq, s.pf.Hash)
		}

	case peer.OpHashSetAnswer:
		h, set, err := peer.DecodeHashSet(p.Payload)
		if err == nil && h == s.pf.Hash {
			if err := s.pf.SetHashSet(set); err != nil {
				s.m.cfg.Debug("%s: bad hashset from %s: %v", s.pf.Name, s.src.key, err)
			}
		}

	case peer.OpQueueRank:
		s.onQueued(int(r.U32()))

	case peer.OpAcceptUploadReq:
		s.setState(StateDownloading)
		s.timeout = transferTimeout
		return s.requestMore()

	case peer.OpOutOfPartReqs:
		// Our upload slot ended; back to the queue.
		s.setState(StateQueued)
		return errDone

	case peer.OpRequestFileName, peer.OpSetReqFileID:
		// The peer asks us for a file; we don't share yet.
		if len(p.Payload) >= 16 {
			return s.c.Send(proto.ProtoEDonkey, peer.OpFileReqAnsNoFil, p.Payload[:16])
		}

	case peer.OpMessage:
		s.m.cfg.Log("message from %s: %s", s.src.key, r.Str16())
	}
	return nil
}

func (s *session) onQueued(rank int) {
	s.m.mu.Lock()
	s.src.State = StateQueued
	s.src.Rank = rank
	s.m.mu.Unlock()
	s.timeout = queuedTimeout
}

// requestMore tops up the pipeline to pipelineBlocks outstanding blocks.
func (s *session) requestMore() error {
	need := pipelineBlocks - len(s.pending)
	if need <= 0 {
		return nil
	}
	blocks := s.pf.Reserve(s.owner, s.avail(), need)
	if len(blocks) == 0 {
		if len(s.pending) == 0 {
			// Nothing left that this peer can give us.
			s.c.Send(proto.ProtoEDonkey, peer.OpCancelTransfer, nil)
			s.setState(StateNoNeededParts)
			return errDone
		}
		return nil
	}
	s.pending = append(s.pending, blocks...)
	req := make([][2]int64, len(blocks))
	for i, b := range blocks {
		req[i] = [2]int64{b.Start, b.End}
	}
	return s.c.RequestParts(s.pf.Hash, s.pf.IsLarge(), req)
}

func (s *session) findPending(start, end int64) (Range, bool) {
	for _, b := range s.pending {
		if start >= b.Start && end <= b.End {
			return b, true
		}
	}
	return Range{}, false
}

func (s *session) onData(p *proto.Packet) error {
	d, err := peer.DecodeDataPart(p)
	if err != nil {
		return err
	}
	if d.Hash != s.pf.Hash {
		return nil
	}

	if d.Compressed {
		blk, ok := s.findPending(d.Start, d.Start+1)
		if !ok || blk.Start != d.Start {
			return nil
		}
		pb := s.packed[d.Start]
		if pb == nil {
			pb = &packedBlock{size: d.PackedSize}
			s.packed[d.Start] = pb
		}
		pb.buf.Write(d.Data)
		if uint32(pb.buf.Len()) < pb.size {
			return nil
		}
		delete(s.packed, d.Start)
		zr, err := zlib.NewReader(&pb.buf)
		if err != nil {
			return fmt.Errorf("compressed block: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(zr, blk.Len()))
		zr.Close()
		if err != nil {
			return fmt.Errorf("compressed block: %w", err)
		}
		return s.store(d.Start, data)
	}

	if _, ok := s.findPending(d.Start, d.End); !ok {
		return nil // stale data for a block we no longer track
	}
	return s.store(d.Start, d.Data)
}

func (s *session) store(off int64, data []byte) error {
	complete, err := s.pf.Write(off, data)
	if err != nil {
		return err
	}
	n := int64(len(data))
	s.f.bytes.Add(n)
	s.m.mu.Lock()
	s.src.Bytes += n
	s.m.mu.Unlock()

	// Drop finished blocks from the pipeline.
	kept := s.pending[:0]
	for _, b := range s.pending {
		if s.pf.Has(b) {
			s.pf.Release(s.owner, b)
		} else {
			kept = append(kept, b)
		}
	}
	s.pending = kept

	if complete {
		s.c.Send(proto.ProtoEDonkey, peer.OpCancelTransfer, nil)
		go s.m.finish(s.f)
		return errDone
	}
	return s.requestMore()
}

func (s *session) releaseAll() {
	for _, b := range s.pending {
		s.pf.Release(s.owner, b)
	}
	s.pending = nil
}
