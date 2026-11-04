package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/proto"
)

// Version constants we advertise. These match a modern eMule-compatible
// client so servers grant us the newer features (zlib, new tags, unicode).
const (
	EDonkeyVersion = 0x3C
	// eMule version encoding: major<<17 | minor<<10 | update<<7
	EMuleVersion = 0<<17 | 50<<10 | 0<<7
)

// Config controls a server connection.
type Config struct {
	UserHash ed2k.Hash
	Nick     string
	Port     uint16 // our TCP listen port (advertised); 0 means none
	Log      func(format string, args ...any)
}

// Status is the server's view of us after login.
type Status struct {
	ClientID   uint32
	HighID     bool
	TCPFlags   uint32
	Users      uint32
	Files      uint32
	ServerName string
	ServerDesc string
}

// Conn is a live connection to an eD2K server.
type Conn struct {
	cfg  Config
	info Info
	conn net.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	status  Status
	lastMsg string // last server message, used to explain login failures
	waiters map[byte][]chan *proto.Packet

	loggedIn chan struct{}
	closed   chan struct{}
	err      error
	once     sync.Once
}

// LowIDLimit: client IDs below this are "low IDs" (we are firewalled).
const LowIDLimit = 0x01000000

// Dial connects to a server and performs the login handshake. It returns
// once the server has assigned us a client ID or ctx expires.
func Dial(ctx context.Context, info Info, cfg Config) (*Conn, error) {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", info.Addr())
	if err != nil {
		return nil, err
	}
	c := &Conn{
		cfg:      cfg,
		info:     info,
		conn:     nc,
		waiters:  map[byte][]chan *proto.Packet{},
		loggedIn: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	go c.readLoop()

	if err := c.sendLogin(); err != nil {
		c.Close()
		return nil, err
	}
	select {
	case <-c.loggedIn:
		return c, nil
	case <-c.closed:
		err := c.err
		if err == nil || errors.Is(err, io.EOF) {
			err = errors.New("server closed the connection during login")
		}
		c.mu.Lock()
		last := c.lastMsg
		c.mu.Unlock()
		if last != "" {
			err = fmt.Errorf("%w (server said: %s)", err, last)
		}
		return nil, err
	case <-ctx.Done():
		c.Close()
		return nil, fmt.Errorf("login timeout: %w", ctx.Err())
	}
}

// Info returns the server we're connected to.
func (c *Conn) Info() Info { return c.info }

// Status returns the latest known connection status.
func (c *Conn) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Close terminates the connection.
func (c *Conn) Close() error {
	c.shutdown(nil)
	return nil
}

// Done is closed when the connection ends; Err() then reports why.
func (c *Conn) Done() <-chan struct{} { return c.closed }
func (c *Conn) Err() error            { return c.err }

func (c *Conn) shutdown(err error) {
	c.once.Do(func() {
		c.err = err
		c.conn.Close()
		close(c.closed)
	})
}

func (c *Conn) send(opcode byte, payload []byte) error {
	p := proto.Packet{Proto: proto.ProtoEDonkey, Opcode: opcode, Payload: payload}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.conn.Write(p.Encode())
	return err
}

func (c *Conn) sendLogin() error {
	var w proto.Writer
	w.Bytes(c.cfg.UserHash[:])
	w.U32(0) // client ID: 0, server assigns one
	w.U16(c.cfg.Port)

	flags := uint32(proto.SrvCapZlib | proto.SrvCapNewTags | proto.SrvCapUnicode | proto.SrvCapLargeFiles)
	tags := []proto.Tag{
		proto.StrTag(proto.CTName, c.cfg.Nick),
		proto.IntTag(proto.CTVersion, EDonkeyVersion),
		proto.IntTag(proto.CTServerFlags, flags),
		proto.IntTag(proto.CTEMuleVersion, EMuleVersion),
	}
	w.U32(uint32(len(tags)))
	for _, t := range tags {
		w.WriteTag(t)
	}
	return c.send(proto.OpLoginRequest, w.B)
}

// readLoop dispatches incoming packets. Packets with a registered waiter
// are delivered there; status packets update state; the rest are logged.
func (c *Conn) readLoop() {
	for {
		// Servers ping-pong rarely; allow long idle periods.
		c.conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		p, err := proto.ReadPacket(c.conn)
		if err != nil {
			c.shutdown(err)
			return
		}
		c.handle(p)
	}
}

func (c *Conn) handle(p *proto.Packet) {
	r := proto.NewReader(p.Payload)
	switch p.Opcode {
	case proto.OpServerMessage:
		msg := r.Str16()
		c.mu.Lock()
		c.lastMsg = strings.TrimSpace(msg)
		c.mu.Unlock()
		c.cfg.Log("server message: %s", msg)
	case proto.OpIDChange:
		id := r.U32()
		var flags uint32
		if r.Len() >= 4 {
			flags = r.U32()
		}
		c.mu.Lock()
		c.status.ClientID = id
		c.status.HighID = id >= LowIDLimit
		c.status.TCPFlags = flags
		c.mu.Unlock()
		kind := "LowID"
		if id >= LowIDLimit {
			kind = "HighID"
		}
		c.cfg.Log("got client ID %d (%s), server flags 0x%X", id, kind, flags)
		select {
		case <-c.loggedIn:
		default:
			close(c.loggedIn)
		}
	case proto.OpServerStatus:
		users, files := r.U32(), r.U32()
		c.mu.Lock()
		c.status.Users, c.status.Files = users, files
		c.mu.Unlock()
		c.cfg.Log("server status: %d users, %d files", users, files)
	case proto.OpServerIdent:
		r.Hash()
		r.U32()
		r.U16()
		tags, _ := r.ReadTags(int(r.U32()))
		l := proto.TagList(tags)
		c.mu.Lock()
		c.status.ServerName = l.Str(proto.STServerName)
		c.status.ServerDesc = l.Str(proto.STDescription)
		c.mu.Unlock()
		c.cfg.Log("server ident: %s (%s)", l.Str(proto.STServerName), l.Str(proto.STDescription))
	case proto.OpReject:
		c.cfg.Log("server rejected our last command")
		c.deliver(p)
	default:
		if !c.deliver(p) {
			c.cfg.Log("unhandled server opcode 0x%02X (%d bytes)", p.Opcode, len(p.Payload))
		}
	}
}

func (c *Conn) deliver(p *proto.Packet) bool {
	c.mu.Lock()
	q := c.waiters[p.Opcode]
	if len(q) == 0 {
		c.mu.Unlock()
		return false
	}
	ch := q[0]
	c.waiters[p.Opcode] = q[1:]
	c.mu.Unlock()
	// Non-blocking: a waiter registered for several opcodes only takes the
	// first reply, and the read loop must never stall.
	select {
	case ch <- p:
	default:
	}
	return true
}

// request sends a packet and waits for a reply with one of the given opcodes.
func (c *Conn) request(ctx context.Context, opcode byte, payload []byte, replies ...byte) (*proto.Packet, error) {
	ch := make(chan *proto.Packet, 1)
	c.mu.Lock()
	for _, op := range replies {
		c.waiters[op] = append(c.waiters[op], ch)
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		for _, op := range replies {
			q := c.waiters[op]
			for i, w := range q {
				if w == ch {
					c.waiters[op] = append(q[:i:i], q[i+1:]...)
					break
				}
			}
		}
		c.mu.Unlock()
	}()

	if err := c.send(opcode, payload); err != nil {
		return nil, err
	}
	select {
	case p := <-ch:
		return p, nil
	case <-c.closed:
		return nil, fmt.Errorf("connection closed: %v", c.err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Search runs a keyword search and returns the first page of results.
// more reports whether QueryMore can fetch additional results.
func (c *Conn) Search(ctx context.Context, q Query) (results []Result, more bool, err error) {
	payload, err := q.encode()
	if err != nil {
		return nil, false, err
	}
	p, err := c.request(ctx, proto.OpSearchRequest, payload, proto.OpSearchResult, proto.OpReject)
	if err != nil {
		return nil, false, err
	}
	if p.Opcode == proto.OpReject {
		return nil, false, errors.New("search rejected by server")
	}
	return parseSearchResults(p.Payload)
}

// QueryMore requests the next page of results for the last search.
func (c *Conn) QueryMore(ctx context.Context) ([]Result, bool, error) {
	p, err := c.request(ctx, proto.OpQueryMoreResult, nil, proto.OpSearchResult)
	if err != nil {
		return nil, false, err
	}
	return parseSearchResults(p.Payload)
}

// Source is a peer that has (part of) a file.
type Source struct {
	ID   uint32 // IP for HighID, client ID for LowID
	Port uint16
}

func (s Source) LowID() bool { return s.ID < LowIDLimit }

func (s Source) Addr() string {
	if s.LowID() {
		return fmt.Sprintf("lowid:%d", s.ID)
	}
	return net.JoinHostPort(IPFromU32(s.ID).String(), fmt.Sprint(s.Port))
}

// GetSources asks the server for peers sharing the given file.
func (c *Conn) GetSources(ctx context.Context, h ed2k.Hash, size uint64) ([]Source, error) {
	var w proto.Writer
	w.Bytes(h[:])
	if size > 0xFFFFFFFF {
		w.U32(0)
		w.U64(size)
	} else {
		w.U32(uint32(size))
	}
	p, err := c.request(ctx, proto.OpGetSources, w.B, proto.OpFoundSources)
	if err != nil {
		return nil, err
	}
	r := proto.NewReader(p.Payload)
	r.Hash()
	n := int(r.U8())
	srcs := make([]Source, 0, n)
	for i := 0; i < n && r.Err() == nil; i++ {
		srcs = append(srcs, Source{ID: r.U32(), Port: r.U16()})
	}
	return srcs, r.Err()
}

// GetServerList asks the server for other servers it knows about.
func (c *Conn) GetServerList(ctx context.Context) ([]Info, error) {
	p, err := c.request(ctx, proto.OpGetServerList, nil, proto.OpServerList)
	if err != nil {
		return nil, err
	}
	r := proto.NewReader(p.Payload)
	n := int(r.U8())
	var out []Info
	for i := 0; i < n && r.Err() == nil; i++ {
		out = append(out, Info{IP: IPFromU32(r.U32()), Port: r.U16()})
	}
	return out, r.Err()
}
