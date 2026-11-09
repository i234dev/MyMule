package server

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/netutil"
	"mymule/internal/obfus"
	"mymule/internal/proto"
	"mymule/internal/safe"
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
	// Crypt is the obfuscation we offer (crypt options: 1 supports, 2
	// requests, 4 requires): the server then gives us the user hash and
	// crypt options of sources, which obfuscated connections need.
	Crypt byte
	Log   func(format string, args ...any)
	// OnCallback is invoked when another client asks, through the server,
	// that we connect to it. Servers send this to LowID clients, which
	// cannot be connected to; the IP and port come from the server and
	// must be checked before use. userHash and crypt are zero when the
	// server does not give them.
	OnCallback func(ip net.IP, port uint16, userHash ed2k.Hash, crypt byte)
	// Blocked, when set, reports addresses not to connect to (the IP
	// filter): Dial refuses such a server.
	Blocked func(ip net.IP) bool
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
	waiters []*waiter
	// idSet: the server gave us an ID; idRefused: it then tried to change
	// it (said once in the log).
	idSet, idRefused bool

	// Lines the server made us log in the minute since logWindow, and
	// those left out.
	logWindow            time.Time
	logLines, logDropped int

	// Search replies carry no request id. A search given up on (timeout)
	// may still be answered later; that answer must not be taken for the
	// next search's. staleSearch counts such pending answers, until
	// staleUntil; searchMu runs one search at a time.
	searchMu    sync.Mutex
	staleSearch int
	staleUntil  time.Time

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
	if cfg.Blocked != nil && cfg.Blocked(info.IP) {
		return nil, fmt.Errorf("%s: blocked by the IP filter", info.Addr())
	}
	nc, err := dialServer(ctx, info, cfg)
	if err != nil {
		return nil, err
	}
	c := &Conn{
		cfg:      cfg,
		info:     info,
		conn:     nc,
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

// readIdle is how long the server may stay silent before the connection
// is checked with a keep-alive (a variable so tests can shorten it).
var readIdle = 10 * time.Minute

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *Conn) shutdown(err error) {
	c.once.Do(func() {
		c.err = err
		c.conn.Close()
		close(c.closed)
	})
}

func (c *Conn) send(opcode byte, payload []byte) error {
	return c.sendProto(proto.ProtoEDonkey, opcode, payload)
}

func (c *Conn) sendProto(protocol, opcode byte, payload []byte) error {
	p := proto.Packet{Proto: protocol, Opcode: opcode, Payload: payload}
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
	if c.cfg.Crypt&1 != 0 {
		flags |= proto.SrvCapSupportCry
	}
	if c.cfg.Crypt&2 != 0 {
		flags |= proto.SrvCapRequestCry
	}
	if c.cfg.Crypt&4 != 0 {
		flags |= proto.SrvCapRequireCry
	}
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

// dialServer connects to a server: obfuscated, on its obfuscation port,
// when it has one and we offer obfuscation; otherwise, or if that fails
// and we do not require it, plainly.
func dialServer(ctx context.Context, info Info, cfg Config) (net.Conn, error) {
	var d net.Dialer
	if cfg.Crypt&1 != 0 && info.ObfuTCPPort != 0 {
		addr := net.JoinHostPort(info.IP.String(), strconv.Itoa(int(info.ObfuTCPPort)))
		nc, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			stop := context.AfterFunc(ctx, func() { nc.Close() })
			c, herr := obfus.Server(nc)
			stop()
			if herr == nil {
				return c, nil
			}
			nc.Close()
			err = herr
		}
		if cfg.Crypt&4 != 0 {
			return nil, fmt.Errorf("obfuscated connection: %w", err)
		}
		cfg.Log("  obfuscated connection failed (%v); connecting without", err)
	} else if cfg.Crypt&4 != 0 {
		return nil, errors.New("obfuscation is required and this server does not offer it")
	}
	return d.DialContext(ctx, "tcp", info.Addr())
}

// Obfuscated reports whether the connection to the server is obfuscated.
func (c *Conn) Obfuscated() bool {
	oc, ok := c.conn.(*obfus.Conn)
	return ok && oc.Obfuscated()
}

// readLoop dispatches incoming packets. Packets with a registered waiter
// are delivered there; status packets update state; the rest are logged.
func (c *Conn) readLoop() {
	cr := &countingReader{r: c.conn}
	for {
		c.conn.SetReadDeadline(time.Now().Add(readIdle))
		cr.n = 0
		p, err := proto.ReadPacket(cr)
		if err != nil {
			// Servers can stay silent for a long time when they have
			// nothing to say; that used to drop the connection every few
			// minutes. If nothing at all arrived, check the connection by
			// sending a keep-alive instead: it fails if the connection is
			// dead. (A timeout in the middle of a packet still closes it.)
			var ne net.Error
			if cr.n == 0 && errors.As(err, &ne) && ne.Timeout() && c.KeepAlive() == nil {
				continue
			}
			c.shutdown(err)
			return
		}
		// A panic caused by what the server sent drops this connection
		// (MyMule reconnects) instead of the whole program.
		if safe.Catch(c.cfg.Log, "packet from server "+c.info.Addr(), func() { c.handle(p) }, &c.mu) {
			c.shutdown(errors.New("internal error while handling a server packet"))
			return
		}
	}
}

// testHandleHook, when set by a test, runs before every server packet.
var testHandleHook func(*proto.Packet)

func (c *Conn) handle(p *proto.Packet) {
	if testHandleHook != nil {
		testHandleHook(p)
	}
	r := proto.NewReader(p.Payload)
	switch p.Opcode {
	case proto.OpServerMessage:
		msg := netutil.CleanText(r.Str16(), 1024)
		c.mu.Lock()
		c.lastMsg = strings.TrimSpace(msg)
		c.mu.Unlock()
		c.logServer("server message: %s", msg)
	case proto.OpIDChange:
		id := r.U32()
		var flags uint32
		if r.Len() >= 4 {
			flags = r.U32()
		}
		c.mu.Lock()
		// Our ID (our public IP when HighID) is set once per connection;
		// only a LowID may become a HighID (the server checked our port).
		// A server changing it at will would make others' Kad keys for us
		// useless, or give us someone else's address as ours.
		if c.idSet && id != c.status.ClientID && !(c.status.ClientID < LowIDLimit && id >= LowIDLimit) {
			log := !c.idRefused
			c.idRefused = true
			c.mu.Unlock()
			if log {
				c.cfg.Log("server tried to change our client ID from %d to %d; ignored", c.status.ClientID, id)
			}
			break
		}
		c.idSet = true
		c.status.ClientID = id
		c.status.HighID = id >= LowIDLimit
		c.status.TCPFlags = flags
		c.mu.Unlock()
		kind := "LowID"
		if id >= LowIDLimit {
			kind = "HighID"
		}
		c.logServer("got client ID %d (%s), server flags 0x%X", id, kind, flags)
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
		c.logServer("server status: %d users, %d files", users, files)
	case proto.OpServerIdent:
		r.Hash()
		r.U32()
		r.U16()
		tags, _ := r.ReadTags(int(r.U32()))
		l := proto.TagList(tags)
		c.mu.Lock()
		name := netutil.CleanText(l.Str(proto.STServerName), 128)
		desc := netutil.CleanText(l.Str(proto.STDescription), 256)
		c.status.ServerName = name
		c.status.ServerDesc = desc
		c.mu.Unlock()
		c.logServer("server ident: %s (%s)", name, desc)
	case proto.OpReject:
		c.logServer("server rejected our last command")
		c.deliver(p)
	case proto.OpCallbackRequested:
		ip, port := IPFromU32(r.U32()), r.U16()
		var hash ed2k.Hash
		var crypt byte
		if r.Len() >= 17 {
			crypt = r.U8()
			hash = r.Hash()
		}
		if r.Err() == nil && c.cfg.OnCallback != nil {
			go c.cfg.OnCallback(ip, port, hash, crypt)
		}
	case proto.OpCallbackFail:
		c.logServer("server: callback request failed")
	default:
		if !c.deliver(p) {
			c.logServer("unhandled server opcode 0x%02X (%d bytes)", p.Opcode, len(p.Payload))
		}
	}
}

// waiter is a pending request waiting for a reply packet.
type waiter struct {
	ops   []byte
	match func(*proto.Packet) bool // optional extra filter (e.g. by file hash)
	ch    chan *proto.Packet
}

func (w *waiter) accepts(p *proto.Packet) bool {
	for _, op := range w.ops {
		if op == p.Opcode {
			return w.match == nil || w.match(p)
		}
	}
	return false
}

// staleSearchWait is how long the answer to an abandoned search is
// waited for, to throw it away.
const staleSearchWait = time.Minute

// deliver hands p to the oldest waiter that accepts it.
func (c *Conn) deliver(p *proto.Packet) bool {
	c.mu.Lock()
	if p.Opcode == proto.OpSearchResult && c.staleSearch > 0 {
		if time.Now().Before(c.staleUntil) {
			c.staleSearch--
			c.mu.Unlock()
			return true // the late answer to a search given up on
		}
		c.staleSearch = 0
	}
	var w *waiter
	for i, cand := range c.waiters {
		if cand.accepts(p) {
			w = cand
			c.waiters = append(c.waiters[:i:i], c.waiters[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	if w == nil {
		return false
	}
	w.ch <- p // buffered(1) and removed from the list: never blocks
	return true
}

// request sends a packet and waits for a reply with one of the given
// opcodes (and accepted by match, if non-nil).
func (c *Conn) request(ctx context.Context, opcode byte, payload []byte, match func(*proto.Packet) bool, replies ...byte) (*proto.Packet, error) {
	w := &waiter{ops: replies, match: match, ch: make(chan *proto.Packet, 1)}
	c.mu.Lock()
	c.waiters = append(c.waiters, w)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		for i, cand := range c.waiters {
			if cand == w {
				c.waiters = append(c.waiters[:i:i], c.waiters[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
	}()

	if err := c.send(opcode, payload); err != nil {
		return nil, err
	}
	select {
	case p := <-w.ch:
		return p, nil
	case <-c.closed:
		return nil, fmt.Errorf("connection closed: %v", c.err)
	case <-ctx.Done():
		select {
		case p := <-w.ch: // the answer came at the same moment
			return p, nil
		default:
		}
		return nil, ctx.Err()
	}
}

// staleDrain is how long a new search waits for the late answer to one
// given up on before it is sent; after that the late answer is forgotten,
// so a server that never answered cannot make later searches lose theirs.
var staleDrain = 3 * time.Second

// searchRequest sends a search request (one at a time per connection)
// and waits for its answer.
func (c *Conn) searchRequest(ctx context.Context, opcode byte, payload []byte, replies ...byte) (*proto.Packet, error) {
	c.searchMu.Lock()
	defer c.searchMu.Unlock()
	// Wait for it at most staleDrain, and at most half the time this
	// search has, so a late answer that never comes costs little.
	wait := staleDrain
	if d, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(d)/2)
	}
	deadline := time.Now().Add(wait)
	for {
		c.mu.Lock()
		pending := c.staleSearch > 0 && time.Now().Before(c.staleUntil) && time.Now().Before(deadline)
		if !pending {
			c.staleSearch = 0
		}
		c.mu.Unlock()
		if !pending {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err() // nothing sent: nothing stale
		case <-c.closed:
			return nil, fmt.Errorf("connection closed: %v", c.err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err // given up while waiting its turn: not sent
	}
	p, err := c.request(ctx, opcode, payload, nil, replies...)
	if err != nil {
		c.abandonedSearch(ctx)
	}
	return p, err
}

// abandonedSearch notes that a search request was given up on before its
// answer came (only when the request was sent and our side gave up).
func (c *Conn) abandonedSearch(ctx context.Context) {
	if ctx.Err() == nil {
		return
	}
	c.mu.Lock()
	c.staleSearch++
	c.staleUntil = time.Now().Add(staleSearchWait)
	c.mu.Unlock()
}

// Search runs a keyword search and returns the first page of results.
// more reports whether QueryMore can fetch additional results.
func (c *Conn) Search(ctx context.Context, q Query) (results []Result, more bool, err error) {
	payload, err := q.encode()
	if err != nil {
		return nil, false, err
	}
	p, err := c.searchRequest(ctx, proto.OpSearchRequest, payload, proto.OpSearchResult, proto.OpReject)
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
	p, err := c.searchRequest(ctx, proto.OpQueryMoreResult, nil, proto.OpSearchResult)
	if err != nil {
		return nil, false, err
	}
	return parseSearchResults(p.Payload)
}

// Source is a peer that has (part of) a file.
type Source struct {
	ID   uint32 // IP for HighID, client ID for LowID
	Port uint16
	// From servers that give them: the source's crypt options and user
	// hash, needed to connect to it with obfuscation.
	Crypt    byte
	UserHash ed2k.Hash
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
	sameFile := func(p *proto.Packet) bool { return len(p.Payload) >= 16 && bytes.Equal(p.Payload[:16], h[:]) }
	// Servers that support it give each source's crypt options and user
	// hash, when we ask for them that way.
	op := byte(proto.OpGetSources)
	if c.cfg.Crypt&1 != 0 && c.Status().TCPFlags&SrvTCPObfuscation != 0 {
		op = proto.OpGetSourcesObfu
	}
	p, err := c.request(ctx, op, w.B, sameFile, proto.OpFoundSources, proto.OpFoundSourcesObfu)
	if err != nil {
		return nil, err
	}
	return decodeSources(p)
}

func decodeSources(p *proto.Packet) ([]Source, error) {
	r := proto.NewReader(p.Payload)
	r.Hash()
	n := int(r.U8())
	srcs := make([]Source, 0, n)
	for i := 0; i < n && r.Err() == nil; i++ {
		s := Source{ID: r.U32(), Port: r.U16()}
		if p.Opcode == proto.OpFoundSourcesObfu {
			s.Crypt = r.U8()
			if s.Crypt&0x80 != 0 {
				s.UserHash = r.Hash()
			}
			s.Crypt &= 0x7F
		}
		srcs = append(srcs, s)
	}
	return srcs, r.Err()
}

// GetServerList asks the server for other servers it knows about.
func (c *Conn) GetServerList(ctx context.Context) ([]Info, error) {
	p, err := c.request(ctx, proto.OpGetServerList, nil, nil, proto.OpServerList)
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

// KeepAlive sends an empty file offer, which is what eMule uses to keep
// the server connection from being dropped as idle.
func (c *Conn) KeepAlive() error {
	var w proto.Writer
	w.U32(0)
	return c.send(proto.OpOfferFiles, w.B)
}

// RequestCallback asks the server to tell a LowID client to connect to us.
// Only works when we are HighID and the peer is on the same server.
func (c *Conn) RequestCallback(lowID uint32) error {
	var w proto.Writer
	w.U32(lowID)
	return c.send(proto.OpCallbackRequest, w.B)
}

// Server TCP capability flags (sent with OP_IDCHANGE).
const (
	SrvTCPCompression = 0x0001
	SrvTCPNewTags     = 0x0008
	SrvTCPUnicode     = 0x0010
	SrvTCPLargeFiles  = 0x0100
	SrvTCPObfuscation = 0x0400 // gives sources' crypt options and user hashes
)

// Offer is a shared file announced to the server.
type Offer struct {
	Hash     ed2k.Hash
	Name     string
	Size     uint64
	Type     string // server file type (Audio, Video, ...), optional
	Complete bool
}

// maxOffered caps one announcement; servers enforce their own limits too.
const maxOffered = 1000

// OfferFiles announces our shared files so the server can hand us out as
// a source. Each call replaces what the server knows about us.
func (c *Conn) OfferFiles(files []Offer) error {
	st := c.Status()
	if len(files) > maxOffered {
		files = files[:maxOffered]
	}
	var body proto.Writer
	count := 0
	for _, f := range files {
		if f.Size > 0xFFFFFFFF && st.TCPFlags&SrvTCPLargeFiles == 0 {
			continue // this server cannot index files over 4 GB
		}
		count++
		body.Bytes(f.Hash[:])
		switch {
		case st.TCPFlags&SrvTCPCompression != 0:
			// Modern servers: special IDs tell complete from partial and
			// work for LowID too; the server fills in our real address.
			if f.Complete {
				body.U32(0xFBFBFBFB)
				body.U16(0xFBFB)
			} else {
				body.U32(0xFCFCFCFC)
				body.U16(0xFCFC)
			}
		case st.HighID:
			body.U32(st.ClientID)
			body.U16(c.cfg.Port)
		default:
			body.U32(0)
			body.U16(0)
		}
		tags := []proto.Tag{
			proto.StrTag(proto.FTFileName, f.Name),
			proto.IntTag(proto.FTFileSize, uint32(f.Size)),
		}
		if f.Size > 0xFFFFFFFF {
			tags = append(tags, proto.IntTag(proto.FTFileSizeHi, uint32(f.Size>>32)))
		}
		if f.Type != "" {
			tags = append(tags, proto.StrTag(proto.FTFileType, f.Type))
		}
		body.U32(uint32(len(tags)))
		for _, t := range tags {
			body.WriteTag(t)
		}
	}
	var w proto.Writer
	w.U32(uint32(count))
	w.Bytes(body.B)

	if st.TCPFlags&SrvTCPCompression != 0 && len(w.B) > 1024 {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(w.B)
		zw.Close()
		if z.Len() < len(w.B) {
			return c.sendProto(proto.ProtoPacked, proto.OpOfferFiles, z.Bytes())
		}
	}
	return c.send(proto.OpOfferFiles, w.B)
}

// FileTypeFor guesses the server file type from a file name.
func FileTypeFor(name string) string {
	ext := strings.ToLower(name)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		ext = ext[i+1:]
	} else {
		return ""
	}
	switch ext {
	case "mp3", "flac", "ogg", "wav", "aac", "m4a", "wma", "opus", "ape":
		return TypeAudio
	case "avi", "mkv", "mp4", "mpg", "mpeg", "mov", "wmv", "webm", "m4v", "ts", "divx", "ogm":
		return TypeVideo
	case "jpg", "jpeg", "png", "gif", "bmp", "webp", "tif", "tiff":
		return TypeImage
	case "exe", "msi", "apk", "dmg", "deb", "rpm", "appimage":
		return TypeProgram
	case "pdf", "epub", "doc", "docx", "txt", "odt", "mobi", "djvu", "rtf", "chm":
		return TypeDocument
	case "zip", "rar", "7z", "gz", "tgz", "bz2", "xz", "tar":
		return TypeArchive
	case "iso", "img", "bin", "cue", "nrg", "mdf":
		return TypeCDImage
	}
	return ""
}

// maxServerLogs is how many lines a server may make us log per minute
// (messages, status, unknown packets...): a server sending them without
// end must not fill the disk.
const maxServerLogs = 20

// logServer logs a line caused by what the server sent, within
// maxServerLogs a minute; the lines left out are counted and said.
func (c *Conn) logServer(format string, args ...any) {
	now := time.Now()
	c.mu.Lock()
	dropped := 0
	if now.Sub(c.logWindow) >= time.Minute {
		dropped = c.logDropped
		c.logWindow, c.logLines, c.logDropped = now, 0, 0
	}
	if c.logLines >= maxServerLogs {
		c.logDropped++
		c.mu.Unlock()
		return
	}
	c.logLines++
	c.mu.Unlock()
	if dropped > 0 {
		c.cfg.Log("server: %d more lines from it not logged", dropped)
	}
	c.cfg.Log(format, args...)
}
