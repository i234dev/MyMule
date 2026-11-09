package download

import (
	"context"
	"errors"
	"net"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/netutil"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/safe"
)

const (
	askTimeout     = 40 * time.Second
	pipelineBlocks = 3
)

// Session time limits (variables for the tests).
var (
	queuedTimeout   = 15 * time.Minute // stay connected while the peer allows it
	transferTimeout = 100 * time.Second
	askLimit        = 2 * time.Minute // whole asking phase (name, status, hash set, rank)
)

// handshakeTimeout is how long a new connection has, in total, to
// complete the hello exchange. (A variable so tests can shorten it.)
var handshakeTimeout = 30 * time.Second

var errDone = errors.New("session finished")

// errCompressed ends a session whose peer sends compressed blocks, which
// we do not support and never ask for.
var errBanned = errors.New("peer sent corrupt data")

var errStateTimeout = errors.New("peer took too long without progress")

var errCompressed = errors.New("peer sent a compressed block, which we do not accept")

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

	// The session runs inside safe.Catch, and everything it must undo is
	// undone afterwards, here, not in defers inside it: after a panic the
	// recovery (which checks that no lock was left held) must come first,
	// and the cleanup must still run, or the source would stay busy and
	// its blocks reserved for good.
	var cl sessionCleanup
	panicked := safe.Catch(m.cfg.Log, "peer session with "+nc.RemoteAddr().String(), func() {
		m.session(ctx, c, nc, outbound, src, &cl)
	}, m.locks()...)
	m.afterSession(ctx, c, nc, &cl, panicked)
}

// afterSession undoes what a session left (see runSession).
func (m *Manager) afterSession(ctx context.Context, c *peer.Conn, nc net.Conn, cl *sessionCleanup, panicked bool) {
	if cl.s != nil {
		cl.s.releaseAll()
	}
	// Release the source before serving the peer: the upload phase can
	// last many minutes, and the source must not stay busy (and count
	// against MaxConns) all that time.
	if cl.src != nil {
		m.releaseSource(cl.src)
	}
	if cl.serveAfter && !panicked {
		// Its own panic containment: this runs after safe.Catch above.
		safe.Catch(m.cfg.Log, "serving peer "+nc.RemoteAddr().String(), func() {
			m.serveUploads(ctx, c, cl.hello)
		}, m.locks()...)
	}
	if cl.uploadConnected {
		m.cfg.Upload.Disconnected(c)
	}
}

// sessionCleanup records what runSession must undo when a session ends.
type sessionCleanup struct {
	hello           peer.Hello
	uploadConnected bool     // Upload.Connected was called
	src             *Source  // claimed or dialed source, to release
	s               *session // download session, whose blocks to release
	serveAfter      bool     // the peer may still download from us
}

func (m *Manager) session(ctx context.Context, c *peer.Conn, nc net.Conn, outbound bool, src *Source, cl *sessionCleanup) {
	if src != nil {
		cl.src = src // dialed: ours to release whatever happens
	}
	if testSessionHook != nil {
		testSessionHook()
	}

	hello, err := m.handshake(c, outbound)
	cl.hello = hello
	if err == nil && m.cfg.Upload != nil {
		m.cfg.Upload.Connected(c, hello, c.RemoteIP())
		cl.uploadConnected = true
	}
	if err != nil {
		m.cfg.Debug("[%s]: handshake failed: %v", nc.RemoteAddr(), err)
		if src != nil {
			m.mu.Lock()
			src.Fails++
			src.State = StateUnreachable
			src.NextAsk = time.Now().Add(time.Duration(src.Fails) * 5 * time.Minute)
			m.mu.Unlock()
		}
		return
	}

	if m.cfg.OnPeerHello != nil {
		m.cfg.OnPeerHello(c.RemoteIP(), hello)
	}
	m.startIdent(c, hello)
	if src == nil && !outbound && m.incomingBuddy(ctx, c, hello) {
		return // the firewalled Kad node we are the buddy of
	}
	if src == nil {
		src = m.claimSource(hello, c.RemoteIP())
		if src == nil {
			// A peer that wants something from us, or one whose upload
			// turn has come and we dialed.
			m.serveUploads(ctx, c, hello)
			return
		}
		cl.src = src
	}

	m.bindConn(c, src.file)
	m.mu.Lock()
	src.UserHash = hello.UserHash
	src.Crypt = hello.Crypt()
	src.Name = hello.Name()
	src.Fails = 0
	f := src.file
	m.mu.Unlock()

	s := &session{m: m, c: c, src: src, f: f, pf: f.pf, owner: src.key, hello: hello}
	cl.s = s
	err = s.run(ctx)
	s.releaseAll()
	// The peer may still be downloading from us on this connection.
	if m.cfg.Upload != nil && m.cfg.Upload.Serving(c) && ctx.Err() == nil {
		cl.serveAfter = true
	}

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
	// One deadline for the whole exchange: a per-read timeout let a peer
	// keep a connection that never identifies itself open for minutes by
	// sending a packet now and then.
	deadline := time.Now().Add(handshakeTimeout)
	for i := 0; i < 4; i++ {
		left := time.Until(deadline)
		if left <= 0 {
			return peer.Hello{}, errors.New("no hello received in time")
		}
		p, err := c.Read(left)
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
			if m.cfg.KadBuddyOffer != nil && m.cfg.KadBuddyOffer(c.RemoteIP(), h.UserHash) {
				ours = ours.WithUDPPort(ours.KadPort())
			}
			return h, c.Send(proto.ProtoEDonkey, peer.OpHelloAnswer, ours.Encode(true))
		case p.Proto == proto.ProtoEMule && p.Opcode == peer.OpEMuleInfo:
			c.Send(proto.ProtoEMule, peer.OpEMuleInfoAnswer, peer.EMuleInfoAnswer())
		}
	}
	return peer.Hello{}, errors.New("no hello received")
}

// serveUploads handles a connection from a peer that only wants files
// from us. It stays open while the peer uses an upload slot and closes
// after a minute of silence otherwise.
//
// A connection we are not uploading on is kept for unservedLimit at most,
// whatever the peer sends: otherwise any peer could hold one of our
// incoming connection slots for ever with a packet every minute.
func (m *Manager) serveUploads(ctx context.Context, c *peer.Conn, hello peer.Hello) {
	if testServeHook != nil {
		testServeHook()
	}
	up := m.cfg.Upload
	usefulAt := time.Now()
	for ctx.Err() == nil {
		timeout := 60 * time.Second
		if up != nil && up.Serving(c) {
			timeout = 2 * time.Minute
			usefulAt = time.Now()
		} else if time.Since(usefulAt) > unservedLimit {
			return
		}
		p, err := c.Read(timeout)
		if err != nil {
			return
		}
		if p.Proto == proto.ProtoEMule && p.Opcode == peer.OpEMuleInfo {
			c.Send(proto.ProtoEMule, peer.OpEMuleInfoAnswer, peer.EMuleInfoAnswer())
			continue
		}
		if m.kadPacket(c, p) || m.sxPacket(c, hello, p) {
			continue
		}
		if ok, err := m.identPacket(c, p); ok {
			if err != nil {
				return
			}
			continue
		}
		if up != nil {
			if _, err := up.Handle(c, hello, c.RemoteIP(), p); err != nil {
				return
			}
			continue
		}
		// Not sharing: say we do not have it.
		if p.Proto == proto.ProtoEDonkey && (p.Opcode == peer.OpRequestFileName || p.Opcode == peer.OpSetReqFileID) && len(p.Payload) >= 16 {
			c.Send(proto.ProtoEDonkey, peer.OpFileReqAnsNoFil, p.Payload[:16])
		}
	}
}

// unservedLimit is how long a peer connection may stay open without us
// uploading on it (asking for files and its queue place takes seconds).
var unservedLimit = 3 * time.Minute

// testServeHook, when set by a test, runs at the start of serveUploads.
var testServeHook func()

type session struct {
	m     *Manager
	c     *peer.Conn
	src   *Source
	f     *File
	pf    *PartFile
	owner string

	pending []Range // blocks requested from this peer
	askedAt time.Time
	timeout time.Duration
	// deadline ends the current state whatever the peer sends: packets
	// that carry nothing useful (an info packet, a repeated queue rank)
	// reset the read timeout but not this, so a peer cannot hold one of
	// our connection slots for ever. Data received moves it forward.
	deadline time.Time
	// The deadline is set when the peer first queues us or first gives
	// us an upload slot, not each time it says so: alternating the two
	// would otherwise hold the connection for ever.
	queued, accepted bool
	hello            peer.Hello
	// sxVersion is the source exchange version we asked this peer with (0
	// = no request pending); sxSecond: with the second form.
	sxVersion byte
	sxSecond  bool
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

// identities are the names a ban of this peer also covers (see
// PartFile.BanIdentities): its address. Not its user hash: a peer says
// what it is, and nothing proves it, so a peer giving the hash of an
// honest source and then sending corrupt parts would get that source
// banned.
func (s *session) identities() []string {
	ids := []string{s.owner}
	if ip := s.c.RemoteIP(); ip != nil {
		ids = append(ids, "ip:"+ip.String())
	} else if host, _, err := net.SplitHostPort(s.owner); err == nil && net.ParseIP(host) != nil {
		ids = append(ids, "ip:"+net.ParseIP(host).String())
	}
	return ids
}

func (s *session) run(ctx context.Context) error {
	h := s.pf.Hash
	if s.pf.BannedAny(s.identities()...) {
		s.setState(StateNoFile)
		return errBanned
	}
	s.setState(StateAsking)
	if err := s.c.SendHash(proto.ProtoEDonkey, peer.OpRequestFileName, h); err != nil {
		return err
	}
	if err := s.c.SendHash(proto.ProtoEDonkey, peer.OpSetReqFileID, h); err != nil {
		return err
	}
	s.timeout = askTimeout
	s.deadline = time.Now().Add(askLimit)

	for ctx.Err() == nil {
		left := time.Until(s.deadline)
		if left <= 0 {
			return errStateTimeout
		}
		p, err := s.c.Read(min(s.timeout, left))
		if err != nil {
			if time.Until(s.deadline) <= 0 {
				return errStateTimeout
			}
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
	if s.m.kadPacket(s.c, p) || s.m.sxPacket(s.c, s.hello, p) {
		return nil
	}
	if p.Proto == proto.ProtoEMule && (p.Opcode == peer.OpAnswerSources || p.Opcode == peer.OpAnswerSources2) {
		s.onSourcesAnswer(p)
		return nil
	}
	if ok, err := s.m.identPacket(s.c, p); ok {
		return err
	}
	// Requests for our files go to the upload side.
	if up := s.m.cfg.Upload; up != nil {
		if handled, err := up.Handle(s.c, s.hello, s.c.RemoteIP(), p); handled {
			return err
		}
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
		// eMule sends size/PartSize+1 parts (one more than there are
		// when the size is an exact multiple); older clients the real
		// number. Anything else is malformed: assume complete, as eMule
		// does for a count of 0.
		switch n := NumParts(s.pf.Size); {
		case avail == nil:
		case len(avail) == n:
		case len(avail) == ed2k.StatusPartCount(s.pf.Size):
			avail = avail[:n]
		default:
			avail = nil
		}
		s.m.mu.Lock()
		s.src.Avail = avail
		s.m.mu.Unlock()
		if !s.pf.HasNeededParts(avail) {
			s.setState(StateNoNeededParts)
			return errDone
		}
		if err := s.askSources(); err != nil {
			return err
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
		if !s.accepted {
			s.accepted = true
			s.deadline = time.Now().Add(transferTimeout)
		}
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
		// Anyone can send these: printable, short and quoted, so a message
		// cannot pose as other log lines, and limited in number.
		s.m.logPeerMessage(s.src.key, netutil.CleanText(r.Str16(), 200))
	}
	return nil
}

func (s *session) onQueued(rank int) {
	s.m.mu.Lock()
	s.src.State = StateQueued
	s.src.Rank = rank
	s.m.mu.Unlock()
	if !s.queued && !s.accepted { // first rank: the queued wait starts
		s.deadline = time.Now().Add(queuedTimeout)
	}
	s.queued = true
	s.timeout = queuedTimeout
}

// requestMore tops up the pipeline to pipelineBlocks outstanding blocks.
func (s *session) requestMore() error {
	s.dropLost()
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
	if d.Compressed {
		// We never announce compression, so honest peers never send it.
		// Accepting it meant buffering in memory as many bytes as the
		// peer claimed (up to 4 GB) before anything could be checked.
		return errCompressed
	}
	if d.Hash != s.pf.Hash {
		return nil
	}

	if _, ok := s.findPending(d.Start, d.End); !ok {
		return nil // stale data for a block we no longer track
	}
	return s.store(d.Start, d.Data)
}

func (s *session) store(off int64, data []byte) error {
	fresh, complete, err := s.pf.writeNew(s.owner, off, data)
	if s.pf.Banned(s.owner) {
		// It sent a part that failed verification all by itself.
		s.pf.BanIdentities(s.identities()...)
		s.m.cfg.Log("%s [%s %q]: sent corrupt data; not downloading this file from it again", s.pf.Name, s.owner, s.src.Name)
		s.m.mu.Lock()
		s.src.State = StateNoFile
		s.m.mu.Unlock()
		return errBanned
	}
	if errors.Is(err, ErrNotReserved) {
		// The block was taken from this peer (its part failed or was
		// finished by others). Forget it and ask for something else.
		s.m.cfg.Debug("%s [%s]: dropped data for a block it no longer holds", s.pf.Name, s.owner)
		s.dropLost()
		return s.requestMore()
	}
	if err != nil {
		return err
	}
	n := int64(len(data))
	// Progress: the deadline moves by what the data is worth at
	// minPeerRate (up to transferTimeout ahead), so a peer that sends a
	// byte now and then does not keep the connection.
	s.deadline = s.deadline.Add(credit(int(fresh)))
	if limit := time.Now().Add(transferTimeout); s.deadline.After(limit) {
		s.deadline = limit
	}
	s.f.bytes.Add(n)
	if cr := s.m.cfg.Credits; cr != nil {
		// Only what is new to us: bytes sent again would let a peer earn
		// the best place in our upload queue for nothing.
		cr.Downloaded(s.hello.UserHash, s.c.RemoteIP(), int64(fresh))
	}
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
		s.m.goFinish(s.f)
		return errDone
	}
	return s.requestMore()
}

// dropLost removes from the pipeline the blocks that are no longer
// reserved for this peer, so it neither waits for them nor writes them.
func (s *session) dropLost() {
	kept := s.pending[:0]
	for _, b := range s.pending {
		if s.pf.Owns(s.owner, b) {
			kept = append(kept, b)
		}
	}
	s.pending = kept
}

func (s *session) releaseAll() {
	for _, b := range s.pending {
		s.pf.Release(s.owner, b)
	}
	s.pending = nil
}
