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
	Dial  func(ip net.IP, port uint16)
	Log   func(format string, args ...any)
	Debug func(format string, args ...any)
}

type client struct {
	userHash ed2k.Hash
	name     string
	ip       net.IP
	port     uint16
	lowID    bool
	emule    bool
	file     ed2k.Hash
	conn     *peer.Conn

	queuedAt, lastAsk time.Time
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
	limiter *limiter

	mu      sync.Mutex
	clients map[clientID]*client // by user hash and IP
	queue   []*client            // waiting, in order
	byConn  map[*peer.Conn]*client

	total      atomic.Int64
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
		limiter:   newLimiter(cfg.LimitKBps * 1024),
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
	_, cl.emule = h.Tags.Find(peer.CTEMuleVersion)
}

// Handle processes an upload-related packet. It returns false for packets
// that are not about uploading.
func (m *Manager) Handle(c *peer.Conn, h peer.Hello, ip net.IP, p *proto.Packet) (bool, error) {
	if p.Proto == proto.ProtoEMule && p.Opcode == peer.OpRequestPartsI64 {
		return true, m.onRequestParts(c, p, true)
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
		cl = &client{userHash: h.UserHash, ip: ip, queuedAt: time.Now(), speedTime: time.Now()}
		m.clients[idOf(h.UserHash, ip)] = cl
		m.queue = append(m.queue, cl)
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

func (m *Manager) firstWaitingLocked() *client {
	for _, cl := range m.queue {
		if !cl.uploading && cl.awaiting.IsZero() {
			return cl
		}
	}
	return nil
}

func (m *Manager) rankLocked(cl *client) int {
	for i, q := range m.queue {
		if q == cl {
			return i + 1
		}
	}
	return 0
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
	cl.queuedAt = time.Now()
	m.queue = append(m.queue, cl)
}

// requeueLocked moves a queued client to the end of the queue.
func (m *Manager) requeueLocked(cl *client) {
	for i, q := range m.queue {
		if q == cl {
			m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
			break
		}
	}
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
func (m *Manager) TryLock() bool { return m.mu.TryLock() }
func (m *Manager) Unlock()       { m.mu.Unlock() }

func (m *Manager) tick() {
	now := time.Now()
	var accepts []*peer.Conn
	var outOfParts []*peer.Conn
	type dial struct {
		ip   net.IP
		port uint16
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
		for _, cl := range m.queue {
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
			dials = append(dials, dial{next.ip, next.port})
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
		go m.cfg.Dial(d.ip, d.port)
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

// ---- bandwidth limiter ----

type limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second; 0 = unlimited
	tokens float64
	last   time.Time
}

func newLimiter(bytesPerSec int) *limiter {
	return &limiter{rate: float64(bytesPerSec), tokens: float64(bytesPerSec), last: time.Now()}
}

// Wait blocks until n bytes may be sent.
func (l *limiter) Wait(n int) {
	if l.rate <= 0 {
		return
	}
	for {
		l.mu.Lock()
		now := time.Now()
		// The bucket holds at least one packet: with a limit below the
		// packet size (1-9 KB/s) it could otherwise never fill enough,
		// and every upload hung for good.
		l.tokens = min(max(l.rate, float64(n)), l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= float64(n) {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return
		}
		wait := time.Duration((float64(n) - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		time.Sleep(wait)
	}
}
