// Package upload serves shared files to other peers: it answers file
// requests, keeps the upload queue, hands out a limited number of upload
// slots in turn and sends data within a bandwidth limit.
package upload

import (
	"context"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/netutil"
	"mymule/internal/peer"
	"mymule/internal/proto"
	"mymule/internal/safe"
	"mymule/internal/share"
)

const (
	maxQueue    = 2000
	queueExpiry = time.Hour        // peers must reask within this time
	slotIdle    = 90 * time.Second // slot freed if the peer stops asking for data
	slotQuota   = 10 << 20         // bytes per turn before the slot rotates
	acceptWait  = 40 * time.Second // waiting for a peer we dialed to give it its slot
	slotMaxTime = 15 * time.Minute // a turn ends after this even below slotQuota
	maxDialMiss = 3                // dialed for its slot this often without answer: dropped
	chunkSize   = 10240            // data packet size, as eMule
	maxRange    = 3 * peer.BlockSize
)

// Config configures the upload manager.
type Config struct {
	Slots     int
	LimitKBps int // 0 = unlimited
	Library   *share.Library
	// Dial connects to a queued HighID peer whose turn has come. The
	// connection must perform the hello handshake and then report it
	// through Connected.
	Dial func(ip net.IP, port uint16, userHash ed2k.Hash, crypt byte)
	// Ratio, when set, gives a peer's credit ratio (1 to 10): its waiting
	// time counts that many times in the queue, as in eMule.
	Ratio func(userHash ed2k.Hash, ip net.IP) float64
	// Uploaded, when set, is told every n bytes sent to a peer.
	Uploaded func(userHash ed2k.Hash, ip net.IP, n int64)
	Log      func(format string, args ...any)
	Debug    func(format string, args ...any)
}

type client struct {
	userHash ed2k.Hash
	name     string
	ip       net.IP
	port     uint16
	lowID    bool
	emule    bool
	crypt    byte   // obfuscation it offers, from its hello
	udpPort  uint16 // its eD2K client UDP port (0 = none), from its hello
	udpVer   byte   // its client UDP version
	file     ed2k.Hash
	conn     *peer.Conn

	queuedAt, lastAsk time.Time
	seq               uint64 // order of arrival in the queue, for ties
	uploading         bool
	awaiting          time.Time // dialed for its slot; give up after this
	lastReq           time.Time // last request we sent data for
	slotStart         time.Time
	dialMiss          int   // times dialed for its slot that it did not come
	sent              int64 // this turn
	totalSent         int64

	speedBytes int64
	speedTime  time.Time
	speed      float64
}

// Manager is the upload side of the client.
type Manager struct {
	cfg     Config
	limiter *netutil.Limiter

	mu      sync.Mutex
	clients map[clientID]*client // by user hash and IP
	queue   []*client            // waiting, in no particular order (see orderLocked)
	seq     uint64
	byConn  map[*peer.Conn]*client

	total      atomic.Int64
	recovery   recoveryBudget
	speedBytes int64
	speedTime  time.Time
	speed      float64
}

// New creates an upload manager. Call Run to start handing out slots.
func New(cfg Config) *Manager {
	if cfg.Slots <= 0 {
		cfg.Slots = 3
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Debug == nil {
		cfg.Debug = func(string, ...any) {}
	}
	return &Manager{
		cfg:       cfg,
		limiter:   netutil.NewLimiter(cfg.LimitKBps * 1024),
		clients:   map[clientID]*client{},
		byConn:    map[*peer.Conn]*client{},
		speedTime: time.Now(),
	}
}

// clientID identifies a peer in the queue by user hash and IP. The user
// hash alone is whatever the peer says: keyed by it, anyone could take
// another user's place in the queue by sending that user's hash.
type clientID struct {
	hash ed2k.Hash
	ip   string
}

func idOf(h ed2k.Hash, ip net.IP) clientID { return clientID{h, ip.String()} }

// maxQueuePerIP caps the queue places one IP address may hold: a few
// users behind one router, not one host filling the queue.
const maxQueuePerIP = 3

// Connected tells the manager that a peer finished the hello handshake.
// If that peer was waiting for its slot, the slot starts now.
func (m *Manager) Connected(c *peer.Conn, h peer.Hello, ip net.IP) {
	m.mu.Lock()
	cl := m.clients[idOf(h.UserHash, ip)]
	if cl == nil {
		m.mu.Unlock()
		return
	}
	// A peer may have several connections with us (it downloads from us
	// on one while we download from it on another): all stay in byConn,
	// and the newest is the one we use to reach it.
	cl.conn = c
	m.byConn[c] = cl
	m.updateIdentity(cl, h, ip)
	start := !cl.awaiting.IsZero() && !cl.uploading
	if start {
		m.startSlotLocked(cl)
	}
	m.mu.Unlock()
	if start {
		m.accept(c)
	}
}

// Disconnected releases a peer connection. An interrupted upload frees
// its slot; the peer keeps a place at the end of the queue.
func (m *Manager) Disconnected(c *peer.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cl := m.byConn[c]
	if cl == nil {
		return
	}
	delete(m.byConn, c)
	if cl.conn != c {
		return // not the connection we use for it; nothing changes
	}
	cl.conn = nil
	for oc, o := range m.byConn {
		if o == cl {
			cl.conn = oc // still connected on another one: keep going
			return
		}
	}
	if cl.uploading {
		cl.uploading = false
		m.enqueueLocked(cl)
	}
}

// Serving reports whether c is currently using an upload slot, so the
// connection must be kept open.
func (m *Manager) Serving(c *peer.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cl := m.byConn[c]
	return cl != nil && cl.uploading
}

func (m *Manager) updateIdentity(cl *client, h peer.Hello, ip net.IP) {
	cl.name = h.Name()
	cl.ip = ip
	cl.port = h.Port
	cl.lowID = h.ClientID != 0 && h.ClientID < 0x01000000
	cl.crypt = h.Crypt()
	_, cl.emule = h.Tags.Find(peer.CTEMuleVersion)
	cl.udpPort, cl.udpVer = h.UDPPort(), h.UDPVersion()
}

// Handle processes an upload-related packet. It returns false for packets
// that are not about uploading.
func (m *Manager) Handle(c *peer.Conn, h peer.Hello, ip net.IP, p *proto.Packet) (bool, error) {
	if p.Proto == proto.ProtoEMule && p.Opcode == peer.OpRequestPartsI64 {
		return true, m.onRequestParts(c, p, true)
	}
	if p.Proto == proto.ProtoEMule && (p.Opcode == peer.OpAICHFileHashReq || p.Opcode == peer.OpAICHRequest) {
		return true, m.handleAICH(c, ip, p)
	}
	if p.Proto != proto.ProtoEDonkey {
		return false, nil
	}
	lib := m.cfg.Library
	switch p.Opcode {
	case peer.OpRequestFileName:
		if len(p.Payload) < 16 {
			return true, nil
		}
		var hash ed2k.Hash
		copy(hash[:], p.Payload)
		f := lib.Get(hash)
		if f == nil {
			return true, c.SendHash(proto.ProtoEDonkey, peer.OpFileReqAnsNoFil, hash)
		}
		var w proto.Writer
		w.Bytes(hash[:])
		w.Str16(f.FileName())
		return true, c.Send(proto.ProtoEDonkey, peer.OpReqFileNameAns, w.B)

	case peer.OpSetReqFileID:
		if len(p.Payload) < 16 {
			return true, nil
		}
		var hash ed2k.Hash
		copy(hash[:], p.Payload)
		f := lib.Get(hash)
		if f == nil {
			return true, c.SendHash(proto.ProtoEDonkey, peer.OpFileReqAnsNoFil, hash)
		}
		return true, c.Send(proto.ProtoEDonkey, peer.OpFileStatus, fileStatus(hash, f.FileSize(), f.Avail()))

	case peer.OpHashSetRequest:
		if len(p.Payload) < 16 {
			return true, nil
		}
		var hash ed2k.Hash
		copy(hash[:], p.Payload)
		f := lib.Get(hash)
		if f == nil {
			return true, nil
		}
		parts := f.PartHashes()
		if len(parts) == 0 {
			return true, nil
		}
		var w proto.Writer
		w.Bytes(hash[:])
		w.U16(uint16(len(parts)))
		for _, ph := range parts {
			w.Bytes(ph[:])
		}
		return true, c.Send(proto.ProtoEDonkey, peer.OpHashSetAnswer, w.B)

	case peer.OpStartUploadReq:
		return true, m.onStartUpload(c, h, ip, p)

	case peer.OpRequestParts:
		return true, m.onRequestParts(c, p, false)

	case peer.OpCancelTransfer, peer.OpEndOfDownload:
		m.mu.Lock()
		if cl := m.byConn[c]; cl != nil {
			cl.uploading = false
			m.removeLocked(cl)
		}
		m.mu.Unlock()
		return true, nil
	}
	return false, nil
}

func fileStatus(h ed2k.Hash, size int64, avail []bool) []byte {
	var w proto.Writer
	w.Bytes(h[:])
	if avail == nil {
		w.U16(0) // complete file
		return w.B
	}
	// eMule's count (see ed2k.StatusPartCount): with the real number of
	// parts, eMule rejects the status of files whose size is an exact
	// multiple of the part size. The extra part, if any, is empty.
	n := max(ed2k.StatusPartCount(size), len(avail))
	w.U16(uint16(n))
	bits := make([]byte, (n+7)/8)
	for i, a := range avail {
		if a {
			bits[i/8] |= 1 << (i % 8)
		}
	}
	w.Bytes(bits)
	return w.B
}

func (m *Manager) onStartUpload(c *peer.Conn, h peer.Hello, ip net.IP, p *proto.Packet) error {
	if len(p.Payload) < 16 {
		return nil
	}
	var hash ed2k.Hash
	copy(hash[:], p.Payload)
	if m.cfg.Library.Get(hash) == nil {
		return c.SendHash(proto.ProtoEDonkey, peer.OpFileReqAnsNoFil, hash)
	}

	m.mu.Lock()
	cl := m.clients[idOf(h.UserHash, ip)]
	if cl == nil {
		if len(m.queue) >= maxQueue || m.queuedFromLocked(ip) >= maxQueuePerIP {
			m.mu.Unlock()
			return nil // queue full: eMule stays silent too
		}
		cl = &client{userHash: h.UserHash, ip: ip, speedTime: time.Now()}
		m.clients[idOf(h.UserHash, ip)] = cl
		m.enqueueLocked(cl)
		m.cfg.Debug("upload queue: added %s (%s)", h.Name(), ip)
	}
	m.updateIdentity(cl, h, ip)
	if cl.conn != nil && cl.conn != c {
		delete(m.byConn, cl.conn)
	}
	cl.conn = c
	m.byConn[c] = cl
	cl.file = hash
	cl.lastAsk = time.Now()
	cl.dialMiss = 0 // it came by itself: missed dials long ago no longer count

	accept := cl.uploading
	if !accept && m.freeSlotsLocked() > 0 && m.firstWaitingLocked() == cl {
		m.startSlotLocked(cl)
		accept = true
	}
	rank := m.rankLocked(cl)
	emule := cl.emule
	m.mu.Unlock()

	if accept {
		return m.accept(c)
	}
	return sendRank(c, rank, emule)
}

func sendRank(c *peer.Conn, rank int, emule bool) error {
	var w proto.Writer
	if emule {
		w.U16(uint16(min(rank, 0xFFFF)))
		w.Bytes(make([]byte, 10))
		return c.Send(proto.ProtoEMule, peer.OpQueueRanking, w.B)
	}
	w.U32(uint32(rank))
	return c.Send(proto.ProtoEDonkey, peer.OpQueueRank, w.B)
}

func (m *Manager) accept(c *peer.Conn) error {
	return c.Send(proto.ProtoEDonkey, peer.OpAcceptUploadReq, nil)
}

func (m *Manager) onRequestParts(c *peer.Conn, p *proto.Packet, large bool) error {
	r := proto.NewReader(p.Payload)
	hash := ed2k.Hash(r.Hash())
	var starts, ends [3]uint64
	for i := range starts {
		if large {
			starts[i] = r.U64()
		} else {
			starts[i] = uint64(r.U32())
		}
	}
	for i := range ends {
		if large {
			ends[i] = r.U64()
		} else {
			ends[i] = uint64(r.U32())
		}
	}
	if r.Err() != nil {
		return nil
	}

	// The slot is kept alive only by requests we actually send data for:
	// a peer asking for nothing, or for ranges that do not exist, would
	// otherwise hold it forever.
	m.mu.Lock()
	cl := m.byConn[c]
	ok := cl != nil && cl.uploading
	m.mu.Unlock()
	if !ok {
		return nil // no slot: ignore, as eMule does
	}
	f := m.cfg.Library.Get(hash)
	if f == nil {
		return c.SendHash(proto.ProtoEDonkey, peer.OpFileReqAnsNoFil, hash)
	}
	big := f.FileSize() > peer.LargeFileLimit

	for i := range starts {
		if !validRange(starts[i], ends[i], f.FileSize()) {
			continue
		}
		s, e := int64(starts[i]), int64(ends[i])
		// The whole range (at most maxRange) is read at once: each read
		// opens and checks the file, which per 10 KB packet cost more than
		// sending it. If that fails (a range across a part that is not
		// verified yet), it is read packet by packet, as far as possible.
		whole := make([]byte, e-s)
		if share.ReadFull(f, whole, s) != nil {
			whole = nil
		}
		for off := s; off < e; off += chunkSize {
			n := min(int64(chunkSize), e-off)
			var data []byte
			if whole != nil {
				data = whole[off-s : off-s+n]
			} else {
				data = make([]byte, n)
				if err := share.ReadFull(f, data, off); err != nil {
					m.cfg.Debug("upload: cannot read %s at %d: %v", f.FileName(), off, err)
					break
				}
			}
			m.limiter.Wait(int(n))
			var w proto.Writer
			w.Bytes(hash[:])
			var err error
			if big {
				w.U64(uint64(off))
				w.U64(uint64(off + n))
				w.Bytes(data)
				err = c.Send(proto.ProtoEMule, peer.OpSendingPartI64, w.B)
			} else {
				w.U32(uint32(off))
				w.U32(uint32(off + n))
				w.Bytes(data)
				err = c.Send(proto.ProtoEDonkey, peer.OpSendingPart, w.B)
			}
			if err != nil {
				return err
			}
			m.total.Add(n)
			if m.cfg.Uploaded != nil {
				m.cfg.Uploaded(cl.userHash, cl.ip, n)
			}
			m.mu.Lock()
			cl.sent += n
			cl.totalSent += n
			cl.lastReq = time.Now()
			m.mu.Unlock()
		}
	}

	// Rotate: after its quota, the peer goes back to the end of the queue.
	m.mu.Lock()
	rotate := cl.uploading && (cl.sent >= slotQuota || time.Since(cl.slotStart) > slotMaxTime) && m.othersWaitingLocked(cl)
	if rotate {
		cl.uploading = false
		m.enqueueLocked(cl)
	}
	m.mu.Unlock()
	if rotate {
		return c.Send(proto.ProtoEDonkey, peer.OpOutOfPartReqs, nil)
	}
	return nil
}

// validRange reports whether a requested [start, end) is a real piece of a
// file of size bytes, at most maxRange long. It is checked on the unsigned
// values: converted to int64 first, offsets above 2^63 would be negative
// and could pass the checks.
func validRange(start, end uint64, size int64) bool {
	return size >= 0 && end > start && end <= uint64(size) && end-start <= maxRange
}

// ---- queue bookkeeping (m.mu held) ----

func (m *Manager) uploadingLocked() int {
	n := 0
	for _, cl := range m.clients {
		if cl.uploading || !cl.awaiting.IsZero() {
			n++
		}
	}
	return n
}

func (m *Manager) freeSlotsLocked() int { return m.cfg.Slots - m.uploadingLocked() }

// queuedFromLocked counts the peers from ip in the queue or uploading.
func (m *Manager) queuedFromLocked(ip net.IP) int {
	n := 0
	for _, cl := range m.clients {
		if cl.ip.Equal(ip) {
			n++
		}
	}
	return n
}

// othersWaitingLocked reports whether a peer other than cl waits in the
// queue and could get a slot now (it is connected, or we can dial it).
// Ending cl's turn for peers that cannot take it (LowID peers that are
// not connected) only gave the slot straight back to cl, over and over.
func (m *Manager) othersWaitingLocked(cl *client) bool {
	for _, q := range m.queue {
		if q != cl && !q.uploading && q.awaiting.IsZero() && (q.conn != nil || (!q.lowID && m.cfg.Dial != nil)) {
			return true
		}
	}
	return false
}

// firstWaitingLocked is the next peer to get a slot: the first waiting
// one whose address has no slot yet, so that one address (one machine
// with several user hashes) cannot take every slot while others wait;
// only when every waiting peer shares an address with an upload, the
// first of them.
func (m *Manager) firstWaitingLocked() *client {
	busy := map[string]bool{}
	for _, cl := range m.clients {
		if cl.uploading || !cl.awaiting.IsZero() {
			busy[cl.ip.String()] = true
		}
	}
	var first *client
	for _, cl := range m.orderLocked() {
		if cl.uploading || !cl.awaiting.IsZero() {
			continue
		}
		if !busy[cl.ip.String()] {
			return cl
		}
		if first == nil {
			first = cl
		}
	}
	return first
}

// orderLocked returns the queue in the order slots are given: by score,
// the time waited multiplied by the credit ratio, as in eMule. With equal
// ratios that is the order of arrival.
func (m *Manager) orderLocked() []*client {
	now := time.Now()
	ratio := m.ratiosLocked()
	q := append([]*client(nil), m.queue...)
	sort.SliceStable(q, func(i, j int) bool { return before(q[i], q[j], ratio, now) })
	return q
}

func (m *Manager) ratiosLocked() map[*client]float64 {
	r := make(map[*client]float64, len(m.queue))
	for _, cl := range m.queue {
		r[cl] = 1
		if m.cfg.Ratio != nil {
			r[cl] = m.cfg.Ratio(cl.userHash, cl.ip)
		}
	}
	return r
}

func before(a, b *client, ratio map[*client]float64, now time.Time) bool {
	ra, rb := ratio[a], ratio[b]
	if ra != rb {
		sa, sb := now.Sub(a.queuedAt).Seconds()*ra, now.Sub(b.queuedAt).Seconds()*rb
		if sa != sb {
			return sa > sb
		}
	}
	if !a.queuedAt.Equal(b.queuedAt) {
		return a.queuedAt.Before(b.queuedAt)
	}
	return a.seq < b.seq
}

func (m *Manager) rankLocked(cl *client) int {
	now := time.Now()
	ratio := m.ratiosLocked()
	if _, ok := ratio[cl]; !ok {
		return 0
	}
	rank := 1
	for _, q := range m.queue {
		if q != cl && before(q, cl, ratio, now) {
			rank++
		}
	}
	return rank
}

func (m *Manager) startSlotLocked(cl *client) {
	cl.uploading = true
	cl.awaiting = time.Time{}
	cl.sent = 0
	cl.dialMiss = 0
	cl.lastReq = time.Now()
	cl.slotStart = cl.lastReq
	for i, q := range m.queue {
		if q == cl {
			m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
			break
		}
	}
	m.cfg.Debug("upload: slot for %s", cl.name)
}

func (m *Manager) enqueueLocked(cl *client) {
	for _, q := range m.queue {
		if q == cl {
			return
		}
	}
	m.stampLocked(cl)
	m.queue = append(m.queue, cl)
}

// stampLocked puts cl's waiting time back to zero.
func (m *Manager) stampLocked(cl *client) {
	cl.queuedAt = time.Now()
	m.seq++
	cl.seq = m.seq
}

// requeueLocked moves a queued client to the end of the queue: its
// waiting starts again.
func (m *Manager) requeueLocked(cl *client) {
	for i, q := range m.queue {
		if q == cl {
			m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
			break
		}
	}
	m.stampLocked(cl)
	m.queue = append(m.queue, cl)
}

func (m *Manager) removeLocked(cl *client) {
	for i, q := range m.queue {
		if q == cl {
			m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
			break
		}
	}
	delete(m.clients, idOf(cl.userHash, cl.ip))
	if cl.conn != nil {
		delete(m.byConn, cl.conn)
	}
}

// Run hands out free slots and expires stale entries until ctx ends.
// A panic in one round is logged and the next round runs as usual.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		safe.Catch(m.cfg.Log, "upload scheduler", m.tick, m)
	}
}

// TryLock and Unlock let a recovered panic check that the upload lock
// was not left held (see package safe).
// TotalSent is how many bytes have been sent to peers since start.
func (m *Manager) TotalSent() int64 { return m.total.Load() }

func (m *Manager) TryLock() bool { return m.mu.TryLock() }
func (m *Manager) Unlock()       { m.mu.Unlock() }

func (m *Manager) tick() {
	now := time.Now()
	var accepts []*peer.Conn
	var outOfParts []*peer.Conn
	type dial struct {
		ip    net.IP
		port  uint16
		hash  ed2k.Hash
		crypt byte
	}
	var dials []dial

	ended := map[*client]bool{}
	m.mu.Lock()
	for _, cl := range m.clients {
		switch {
		case cl.uploading && (now.Sub(cl.lastReq) > slotIdle ||
			(now.Sub(cl.slotStart) > slotMaxTime+slotIdle && m.othersWaitingLocked(cl))):
			// Idle, or a turn that never ends (tiny requests that never
			// reach the quota) while others wait.
			cl.uploading = false
			if cl.conn != nil {
				outOfParts = append(outOfParts, cl.conn)
			}
			ended[cl] = true
			m.enqueueLocked(cl)
		case !cl.awaiting.IsZero() && now.After(cl.awaiting):
			// Dialed for its slot and it didn't come: to the end of the
			// queue, so it does not block the head; dropped if it keeps
			// not answering.
			cl.awaiting = time.Time{}
			cl.dialMiss++
			if cl.dialMiss >= maxDialMiss {
				m.removeLocked(cl)
			} else {
				m.requeueLocked(cl)
			}
		case !cl.uploading && cl.awaiting.IsZero() && now.Sub(cl.lastAsk) > queueExpiry:
			m.removeLocked(cl)
		}
	}
	for m.freeSlotsLocked() > 0 {
		var next *client
		for _, cl := range m.orderLocked() {
			// Not a turn just ended: its "out of parts" and a new accept
			// go out from separate goroutines and could arrive swapped,
			// leaving the peer queued while we count it as uploading.
			if cl.uploading || !cl.awaiting.IsZero() || ended[cl] {
				continue
			}
			if cl.conn != nil || (!cl.lowID && m.cfg.Dial != nil) {
				next = cl
				break
			}
		}
		if next == nil {
			break
		}
		if next.conn != nil {
			m.startSlotLocked(next)
			accepts = append(accepts, next.conn)
		} else {
			next.awaiting = now.Add(acceptWait)
			dials = append(dials, dial{next.ip, next.port, next.userHash, next.crypt})
		}
	}
	m.mu.Unlock()

	// Sent from their own goroutines: a peer that does not read could
	// hold a send for a minute, and with it every other slot and dial.
	for _, c := range outOfParts {
		go safe.Catch(m.cfg.Log, "upload: end of turn", func() { c.Send(proto.ProtoEDonkey, peer.OpOutOfPartReqs, nil) }, m)
	}
	for _, c := range accepts {
		go safe.Catch(m.cfg.Log, "upload: accept", func() { m.accept(c) }, m)
	}
	for _, d := range dials {
		go m.cfg.Dial(d.ip, d.port, d.hash, d.crypt)
	}
}

// ---- status ----

// SlotStatus describes one peer we are uploading to.
type SlotStatus struct {
	Name  string  `json:"name"`
	Addr  string  `json:"addr"`
	File  string  `json:"file"`
	Sent  int64   `json:"sent"`
	Speed float64 `json:"speed"`
}

// Status summarises the upload side.
type Status struct {
	Slots    []SlotStatus `json:"slots"`
	MaxSlots int          `json:"max_slots"`
	Queue    int          `json:"queue"`
	Speed    float64      `json:"speed"`
	Total    int64        `json:"total"`
	LimitKB  int          `json:"limit_kb"`
}

// Status returns a snapshot, updating speed estimates.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	st := Status{MaxSlots: m.cfg.Slots, Queue: len(m.queue), Total: m.total.Load(), LimitKB: m.cfg.LimitKBps}
	if dt := now.Sub(m.speedTime).Seconds(); dt >= 0.5 {
		m.speed = smooth(m.speed, float64(st.Total-m.speedBytes)/dt)
		m.speedBytes, m.speedTime = st.Total, now
	}
	st.Speed = m.speed
	for _, cl := range m.clients {
		if !cl.uploading {
			continue
		}
		if dt := now.Sub(cl.speedTime).Seconds(); dt >= 0.5 {
			cl.speed = smooth(cl.speed, float64(cl.totalSent-cl.speedBytes)/dt)
			cl.speedBytes, cl.speedTime = cl.totalSent, now
		}
		name := ""
		if f := m.cfg.Library.Get(cl.file); f != nil {
			name = f.FileName()
		}
		addr := ""
		if cl.ip != nil {
			addr = net.JoinHostPort(cl.ip.String(), itoa(int(cl.port)))
		}
		st.Slots = append(st.Slots, SlotStatus{Name: cl.name, Addr: addr, File: name, Sent: cl.sent, Speed: cl.speed})
	}
	sort.Slice(st.Slots, func(i, j int) bool { return st.Slots[i].Speed > st.Slots[j].Speed })
	return st
}

func smooth(old, inst float64) float64 { return 0.6*inst + 0.4*old }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// UDPReply is what to send back to a client UDP packet.
type UDPReply struct {
	Op       byte
	Payload  []byte
	UserHash ed2k.Hash // obfuscate for this user hash (zero: plain)
}

// ReaskPing answers OP_REASKFILEPING from ip:port, a peer asking for its
// place in our queue without connecting (eMule's UDP reask): its rank,
// "file not found" or "queue full", or nothing (ok false). The datagram's
// address may be forged; the answer is small and goes to that address,
// and it only keeps a queued peer's place (as its own reask would).
func (m *Manager) ReaskPing(ip net.IP, port uint16, payload []byte) (UDPReply, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cl *client
	for _, c := range m.queue {
		if c.ip.Equal(ip) && c.udpPort == port && port != 0 && !c.uploading {
			cl = c
			break
		}
	}
	v := byte(peer.UDPVersion)
	if cl != nil {
		v = cl.udpVer
	}
	file, err := peer.DecodeReaskFilePing(payload, v)
	if err != nil {
		return UDPReply{}, false
	}
	f := m.cfg.Library.Get(file)
	if f == nil {
		r := UDPReply{Op: peer.OpFileNotFound}
		if cl != nil && cl.crypt&1 != 0 {
			r.UserHash = cl.userHash
		}
		return r, true
	}
	if cl == nil {
		if len(m.queue)+50 > maxQueue {
			return UDPReply{Op: peer.OpQueueFull}, true
		}
		return UDPReply{}, false // it asks over TCP then
	}
	if cl.file != file {
		return UDPReply{}, false // another file than it waits for
	}
	cl.lastAsk = time.Now()
	var status []bool
	if f.Partial() {
		status = StatusBits(f.FileSize(), f.Avail())
	}
	rank := min(m.rankLocked(cl), 0xFFFF)
	r := UDPReply{Op: peer.OpReaskAck, Payload: peer.EncodeReaskAck(cl.udpVer, status, uint16(rank))}
	if cl.crypt&1 != 0 {
		r.UserHash = cl.userHash
	}
	return r, true
}

// StatusBits pads a part status to eMule's part count (see
// ed2k.StatusPartCount; the extra part, if any, is empty).
func StatusBits(size int64, avail []bool) []bool {
	out := make([]bool, max(ed2k.StatusPartCount(size), len(avail)))
	copy(out, avail)
	return out
}
