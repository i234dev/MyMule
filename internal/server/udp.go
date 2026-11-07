package server

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
	"mymule/internal/safe"
)

// UDP opcodes for client <-> server. Server UDP port = TCP port + 4.
const (
	OpGlobGetSources2  = 0x94 // [hash][u32 size] or [hash][u32 0][u64 size], repeated
	OpGlobServStatReq  = 0x96
	OpGlobServStatRes  = 0x97
	OpGlobSearchReq    = 0x98 // [search expression]
	OpGlobSearchRes    = 0x99 // [hash][u32 id][u16 port][u32 ntags][tags]...
	OpGlobGetSources   = 0x9A // [hash]...
	OpGlobFoundSources = 0x9B // [hash][u8 count][(u32 id, u16 port)...]
)

// UDP capability flags (ST_UDPFLAGS in server.met).
const (
	SrvUDPGetSources  = 0x01
	SrvUDPGetSources2 = 0x20
	SrvUDPLargeFiles  = 0x100
)

// FileReq identifies a file we want sources for.
type FileReq struct {
	Hash ed2k.Hash
	Size uint64
}

// maxFilesPerUDPRequest keeps datagrams small; eMule uses a similar limit.
const maxFilesPerUDPRequest = 30

// udpReplyWindow is how long after we last sent to a server we accept
// datagrams from its IP. Replies arrive within seconds; this is generous.
const udpReplyWindow = 10 * time.Minute

// UDP is a socket for talking to many servers at once without logging in.
type UDP struct {
	conn *net.UDPConn

	// Log reports problems (a recovered panic); nil = silent.
	Log func(format string, args ...any)

	// OnSources is called for every OP_GLOBFOUNDSOURCES record.
	OnSources func(from *net.UDPAddr, h ed2k.Hash, srcs []Source)

	mu       sync.Mutex
	searches map[int]*udpSearch
	nextID   int
	asked    map[string]time.Time // server IP -> last datagram we sent it
}

// ListenUDP opens the UDP socket on a random local port, on host (empty
// means all interfaces).
func ListenUDP(host string) (*UDP, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(host)})
	if err != nil {
		return nil, err
	}
	return &UDP{conn: c, searches: map[int]*udpSearch{}, asked: map[string]time.Time{}}, nil
}

// udpSearch is a global search in progress. Answers carry no search id,
// so an answer from a server goes to the search that asked that server
// most recently, and only once it has asked it: an answer arriving
// before that is a late one to an earlier search. When several searches
// asked that server, an answer goes to the newest one it fits (see
// Query.fits), so a late answer to one search does not end up in another
// that runs at the same time (the *arr apps search several titles at once).
type udpSearch struct {
	q    Query
	cb   func(from *net.UDPAddr, r Result)
	sent map[string]time.Time // server IP -> when this search asked it
}

// UDPAddr is the UDP address of a server.
func (s Info) UDPAddr() *net.UDPAddr {
	return &net.UDPAddr{IP: s.IP, Port: int(s.Port) + 4}
}

func (u *UDP) Close() error { return u.conn.Close() }

func (u *UDP) send(to *net.UDPAddr, opcode byte, payload []byte) error {
	u.noteAsked(to.IP, time.Now())
	b := make([]byte, 0, 2+len(payload))
	b = append(b, proto.ProtoEDonkey, opcode)
	b = append(b, payload...)
	_, err := u.conn.WriteToUDP(b, to)
	return err
}

// GetSources asks one server for sources of files, in as few datagrams as
// possible.
func (u *UDP) GetSources(srv Info, files []FileReq) error {
	for len(files) > 0 {
		n := min(len(files), maxFilesPerUDPRequest)
		batch := files[:n]
		files = files[n:]
		var w proto.Writer
		// Modern servers (eserver 17+, eNode) support GETSOURCES2, which
		// carries the size and works for large files. Old servers that
		// declare only the basic flag get the hash-only variant.
		if srv.UDPFlags != 0 && srv.UDPFlags&SrvUDPGetSources2 == 0 {
			for _, f := range batch {
				w.Bytes(f.Hash[:])
			}
			if err := u.send(srv.UDPAddr(), OpGlobGetSources, w.B); err != nil {
				return err
			}
			continue
		}
		for _, f := range batch {
			w.Bytes(f.Hash[:])
			if f.Size > 0xFFFFFFFF {
				w.U32(0)
				w.U64(f.Size)
			} else {
				w.U32(uint32(f.Size))
			}
		}
		if err := u.send(srv.UDPAddr(), OpGlobGetSources2, w.B); err != nil {
			return err
		}
	}
	return nil
}

// Search sends a keyword search to every server and collects results for
// the given duration. onResult is called from the read loop.
func (u *UDP) Search(ctx context.Context, servers []Info, q Query, wait time.Duration, onResult func(from *net.UDPAddr, r Result)) error {
	payload, err := q.encode()
	if err != nil {
		return err
	}
	u.mu.Lock()
	id := u.nextID
	u.nextID++
	srch := &udpSearch{q: q, cb: onResult, sent: map[string]time.Time{}}
	u.searches[id] = srch
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.searches, id)
		u.mu.Unlock()
	}()

	for _, s := range servers {
		addr := s.UDPAddr()
		u.mu.Lock()
		srch.sent[addr.IP.String()] = time.Now()
		u.mu.Unlock()
		u.send(addr, OpGlobSearchReq, payload)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond): // don't burst
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
	return nil
}

// noteAsked records that we sent a datagram to ip, so its replies are
// accepted for udpReplyWindow. Old entries are dropped as we go.
func (u *UDP) noteAsked(ip net.IP, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.asked) > 1000 {
		for k, t := range u.asked {
			if now.Sub(t) > udpReplyWindow {
				delete(u.asked, k)
			}
		}
	}
	u.asked[ip.String()] = now
}

// expected reports whether we recently sent something to ip. Only the
// servers we ask may answer: anyone else could otherwise feed us fake
// sources or search results just by knowing our UDP port. The port is not
// checked because some servers reply from a different one.
func (u *UDP) expected(ip net.IP, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	t, ok := u.asked[ip.String()]
	return ok && now.Sub(t) <= udpReplyWindow
}

// Run reads and dispatches datagrams until the socket is closed.
func (u *UDP) Run() {
	buf := make([]byte, 64*1024)
	for {
		n, from, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := append([]byte(nil), buf[:n]...)
		// One bad datagram must not stop UDP for good (or MyMule).
		safe.Catch(u.Log, "UDP datagram from "+from.String(), func() { u.dispatch(from, data) }, &u.mu)
	}
}

// dispatch handles one datagram, which may hold several packets
// concatenated (each starting with 0xE3 and an opcode).
// testDispatchHook, when set by a test, runs before every datagram.
var testDispatchHook func()

func (u *UDP) dispatch(from *net.UDPAddr, data []byte) {
	if testDispatchHook != nil {
		testDispatchHook()
	}
	if !u.expected(from.IP, time.Now()) {
		return // unsolicited
	}
	r := proto.NewReader(data)
	for r.Len() >= 2 {
		if r.U8() != proto.ProtoEDonkey {
			return
		}
		switch r.U8() {
		case OpGlobFoundSources:
			h := ed2k.Hash(r.Hash())
			n := int(r.U8())
			srcs := make([]Source, 0, n)
			for i := 0; i < n && r.Err() == nil; i++ {
				srcs = append(srcs, Source{ID: r.U32(), Port: r.U16()})
			}
			if r.Err() != nil {
				return
			}
			if u.OnSources != nil {
				u.OnSources(from, h, srcs)
			}
		case OpGlobSearchRes:
			var res Result
			res.Hash = r.Hash()
			r.U32()
			r.U16()
			tags, err := r.ReadTags(int(r.U32()))
			if err != nil {
				return
			}
			fillResult(&res, tags)
			if !validResult(&res) {
				continue
			}
			if cb := u.searchFor(from.IP, res); cb != nil {
				cb(from, res)
			}
		default:
			return // unknown or unneeded (e.g. server status): stop parsing
		}
	}
}

// searchFor returns the callback of the search that should get result r
// from ip, or nil: the newest search that asked ip and that r fits, or,
// when r fits none of them (servers match more loosely than Query.fits),
// the newest search that asked ip.
func (u *UDP) searchFor(ip net.IP, r Result) func(*net.UDPAddr, Result) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var newest, fit *udpSearch
	var newestAt, fitAt time.Time
	for _, s := range u.searches {
		t, ok := s.sent[ip.String()]
		if !ok {
			continue
		}
		if newest == nil || t.After(newestAt) {
			newest, newestAt = s, t
		}
		if s.q.fits(r) && (fit == nil || t.After(fitAt)) {
			fit, fitAt = s, t
		}
	}
	switch {
	case fit != nil:
		return fit.cb
	case newest != nil:
		return newest.cb
	}
	return nil
}

// ServerKey identifies a server by its TCP address, from a UDP reply.
func ServerKey(from *net.UDPAddr) string {
	return net.JoinHostPort(from.IP.String(), strconv.Itoa(from.Port-4))
}
