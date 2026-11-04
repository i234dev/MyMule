package server

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
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

// UDP is a socket for talking to many servers at once without logging in.
type UDP struct {
	conn *net.UDPConn

	// OnSources is called for every OP_GLOBFOUNDSOURCES record.
	OnSources func(from *net.UDPAddr, h ed2k.Hash, srcs []Source)

	mu       sync.Mutex
	searches map[int]func(from *net.UDPAddr, r Result)
	nextID   int
}

// ListenUDP opens the UDP socket on a random local port.
func ListenUDP() (*UDP, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	return &UDP{conn: c, searches: map[int]func(*net.UDPAddr, Result){}}, nil
}

// UDPAddr is the UDP address of a server.
func (s Info) UDPAddr() *net.UDPAddr {
	return &net.UDPAddr{IP: s.IP, Port: int(s.Port) + 4}
}

func (u *UDP) Close() error { return u.conn.Close() }

func (u *UDP) send(to *net.UDPAddr, opcode byte, payload []byte) error {
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
	u.searches[id] = onResult
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.searches, id)
		u.mu.Unlock()
	}()

	for _, s := range servers {
		u.send(s.UDPAddr(), OpGlobSearchReq, payload)
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

// Run reads and dispatches datagrams until the socket is closed.
func (u *UDP) Run() {
	buf := make([]byte, 64*1024)
	for {
		n, from, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		u.dispatch(from, append([]byte(nil), buf[:n]...))
	}
}

// dispatch handles one datagram, which may hold several packets
// concatenated (each starting with 0xE3 and an opcode).
func (u *UDP) dispatch(from *net.UDPAddr, data []byte) {
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
			res.Tags = tags
			fillResult(&res)
			u.mu.Lock()
			cbs := make([]func(*net.UDPAddr, Result), 0, len(u.searches))
			for _, cb := range u.searches {
				cbs = append(cbs, cb)
			}
			u.mu.Unlock()
			for _, cb := range cbs {
				cb(from, res)
			}
		default:
			return // unknown or unneeded (e.g. server status): stop parsing
		}
	}
}

// ServerKey identifies a server by its TCP address, from a UDP reply.
func ServerKey(from *net.UDPAddr) string {
	return net.JoinHostPort(from.IP.String(), strconv.Itoa(from.Port-4))
}
