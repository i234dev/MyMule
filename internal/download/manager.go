package download

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	credits "mymule/internal/credit"
	"mymule/internal/ed2k"
	"mymule/internal/ipfilter"
	"mymule/internal/kad"
	"mymule/internal/netutil"
	"mymule/internal/obfus"
	"mymule/internal/peer"
	"mymule/internal/portmap"
	"mymule/internal/proto"
	"mymule/internal/safe"
	"mymule/internal/server"
	"mymule/internal/share"
	"mymule/internal/stats"
	"mymule/internal/upload"
)

// Timing constants. Reasking a source more often than ~10 minutes gets a
// client flagged as aggressive by eMule, so we stay well above that.
const (
	ReaskInterval     = 20 * time.Minute
	SourceReqInterval = 20 * time.Minute
	ConnectTimeout    = 15 * time.Second
	maxSourceFails    = 5
)

// Config configures a download Manager.
type Config struct {
	UserHash ed2k.Hash
	Nick     string
	Port     uint16 // TCP listen port for incoming peer connections; 0 = none
	// ListenHost restricts the listen sockets to one address (tests use
	// 127.0.0.1); empty means all interfaces, which peers need.
	ListenHost  string
	TempDir     string // where .part files live (needed by AddLink)
	IncomingDir string
	StayAlive   bool // keep running when every download is finished (daemon mode)
	MaxConns    int
	// DownloadLimitKBps limits the file data we take from peers, in all
	// (0 = no limit).
	DownloadLimitKBps int
	// Connect logs into some server with the given config (the manager
	// fills in callbacks). Called again whenever the connection drops.
	Connect func(ctx context.Context, cfg server.Config) (*server.Conn, error)
	// Servers returns the known server list. If set, every server in it
	// is asked for sources over UDP (without logging in), in rotation.
	Servers func() []server.Info
	// Library and Upload enable sharing: partial and finished downloads
	// are shared, offered to the server and served to peers.
	Library *share.Library
	Upload  *upload.Manager
	// Credits, when set, runs eMule's secure identification with every
	// peer and counts what each one uploads to us.
	Credits *credits.Store
	// Totals, when set, gives the all-time upload and download totals,
	// for the status.
	Totals func() stats.Totals
	// Router, when set, says which ports are open on the router (UPnP or
	// NAT-PMP), for the status.
	Router func() *portmap.State
	// SendClientUDP, when set, sends an eD2K client UDP packet from our
	// client UDP port (the Kad port), obfuscated for userHash when not
	// zero. With it, our hello offers client UDP and queued sources are
	// asked for their queue place by UDP.
	SendClientUDP func(to netip.AddrPort, op byte, payload []byte, userHash ed2k.Hash)
	// IPFilter, when set, blocks addresses: no connection from or to them,
	// and they are not taken as sources or servers.
	IPFilter *ipfilter.Holder
	// Obfuscation is the protocol obfuscation we offer, as crypt options
	// (obfus.Supports, Requests, Requires): with Supports, connections to
	// peers that support it are obfuscated, and obfuscated connections
	// to us are accepted; with Requires, nothing else is.
	Obfuscation byte
	Log         func(format string, args ...any) // important events
	Debug       func(format string, args ...any) // per-peer chatter
	Trace       bool                             // log every peer packet via Debug
	OnComplete  func(hash ed2k.Hash, name, path string)
	// OnError is told when a download cannot be finished (once per
	// download; it is tried again later).
	OnError func(name string, err error)

	// OnSilentServer is called once per connection when the connected
	// server (address addr) leaves several searches in a row without an
	// answer; the other servers have been answering them meanwhile.
	OnSilentServer func(addr string)

	// Kad returns our Kad UDP port and version when Kad runs (0, 0 when
	// not), to tell peers in our hello. May be nil.
	Kad func() (port uint16, version byte)
	// KadFirewallAck is called when a peer connected to us to tell us it
	// got through (a Kad firewall check we asked for). May be nil.
	KadFirewallAck func(ip net.IP)
	// KadLegacyAck tells a node of Kad version 6 or older, over UDP to its
	// Kad port, that we got through to its TCP port (newer ones are told
	// over TCP). May be nil.
	KadLegacyAck func(ip net.IP, kadPort uint16)
	// KadSources starts a Kad search for sources of a file; found is
	// called for each source. Nil when Kad is off.
	KadSources func(hash ed2k.Hash, size uint64, found func(kad.Source)) error
	// KadCallback asks a firewalled Kad source (through its buddy, or
	// itself over UDP) to connect to us; we want file from it.
	KadCallback func(src kad.Source, file ed2k.Hash) error
	// KadFirewall says what to tell peers about our reachability; may be
	// nil.
	KadFirewall func() KadFirewall
	// KadUDPCheckRequest runs the Kad UDP firewall test a peer asked us
	// for over TCP (OP_FWCHECKUDPREQ).
	KadUDPCheckRequest func(ip net.IP, payload []byte)
	// KadIncomingBuddy reports whether a peer that connected to us is the
	// firewalled node we agreed to be the buddy of; send reaches it until
	// KadServedBuddyGone.
	KadIncomingBuddy   func(ip net.IP, userHash ed2k.Hash, send func(op byte, payload []byte) bool) bool
	KadServedBuddyGone func(ip net.IP)
	// KadOwnCallback reads an OP_CALLBACK our buddy passed on.
	KadOwnCallback func(payload []byte) (ip netip.Addr, port uint16, file [16]byte, ok bool)
	// KadBuddyOffer reports whether a peer connecting to us is a firewalled
	// node we offered to be the buddy of: our hello then gives it our Kad
	// port as our UDP port, which it hands downloaders.
	KadBuddyOffer func(ip net.IP, userHash ed2k.Hash) bool
	// OnPeerHello is called with every peer's hello (Kad starts from the
	// clients that give their Kad port). May be nil.
	OnPeerHello func(ip net.IP, h peer.Hello)
	// KadStatus returns Kad's state for the status pages; nil when Kad is
	// off.
	KadStatus func() kad.Status
	// KadProblem says why Kad is not running although it is on.
	KadProblem string
	// KadBuddyUp and KadBuddyDown follow the connection to our buddy.
	KadBuddyUp   func(ip net.IP, kadPort uint16)
	KadBuddyDown func(ip net.IP)
	// KadSearch searches Kad for files, calling found for each, until the
	// search ends or ctx is done. Nil when Kad is off.
	KadSearch func(ctx context.Context, q server.Query, found func(server.Result)) error
	// KadNotes asks Kad for the comments and ratings of a file, calling
	// found for each, until the search ends or ctx is done. Nil when Kad
	// is off.
	KadNotes func(ctx context.Context, hash ed2k.Hash, size uint64, found func(kad.Note)) error
	// KadPublishNote publishes our rating and comment of a file in Kad.
	// Nil when Kad is off.
	KadPublishNote func(hash ed2k.Hash, size uint64, name string, rating int, comment string) error

	// SkipFreeSpaceCheck adds downloads even if the disk has no room for
	// them. Only for tests that use large made-up downloads.
	SkipFreeSpaceCheck bool

	// allowPrivateSources accepts server-provided sources on loopback
	// and private addresses. Only tests set it.
	allowPrivateSources bool
}

// Limits on the sources kept per download. Servers and UDP replies can
// hand us any addresses; without limits a single reply could make us
// connect to thousands of ports of one host, and the list never shrank.
const (
	MaxSourcesPerFile = 500 // eMule's default is 400
	MaxSourcesPerIP   = 3   // a few clients on one public IP (NAT) is normal
	// At most this many of a file's sources may be LowID: they can only
	// be reached through their server, and a server could otherwise fill
	// the whole table with made-up ones in two answers.
	MaxLowIDPerFile = MaxSourcesPerFile / 2
)

// SourceState is where we stand with a given peer for a given file.
type SourceState int

const (
	StateNew SourceState = iota
	StateConnecting
	StateAsking
	StateQueued
	StateDownloading
	StateNoNeededParts
	StateNoFile
	StateUnreachable
)

func (s SourceState) String() string {
	return [...]string{"new", "connecting", "asking", "queued", "downloading",
		"no needed parts", "no file", "unreachable"}[s]
}

// Source is a peer that has (part of) a file.
type Source struct {
	key      string
	IP       net.IP
	Port     uint16
	LowID    uint32 // non-zero for firewalled peers
	server   string // server through which a LowID source is reachable
	UserHash ed2k.Hash
	Name     string
	Crypt    byte // crypt options: whether it takes obfuscated connections

	State   SourceState
	Rank    int
	Avail   []bool // nil = unknown or complete file
	NextAsk time.Time
	Fails   int
	Bytes   int64

	busy       bool
	callbackAt time.Time   // when we last asked the server for a callback (LowID)
	udpPort    uint16      // its client UDP port (0 = none), from its hello
	udpVer     byte        // its client UDP version
	udpAskAt   time.Time   // when we asked it by UDP for our queue place
	udpFails   int         // UDP asks in a row it did not answer
	kadFW      *kad.Source // a firewalled Kad source, reached by a callback
	file       *File
}

// File is a download managed by the Manager.
type File struct {
	pf           *PartFile
	sources      map[string]*Source
	lastSrcReq   time.Time
	kadAsked     time.Time // last Kad source search
	bytes        atomic.Int64
	finishing    bool
	finished     bool
	finishTry    time.Time // last attempt to finish (move the file)
	finishFailed bool      // an attempt failed (OnError was told)
	priority     int       // pf's priority, read under m.mu
	paused       bool
	path         string

	speedBytes int64
	speedTime  time.Time
	speed      float64
}

// Manager runs downloads: it keeps a server connection, collects sources,
// schedules peer connections and accepts incoming ones.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	closing bool // shutdown has started
	files   map[ed2k.Hash]*File
	srv     *server.Conn
	// reconnect asks the server loop to connect again right away (another
	// server was chosen), without the usual pause.
	reconnect bool
	active    int
	conns     map[*peer.Conn]*File // live peer connections, and the file they serve (once known)
	idents    map[*peer.Conn]*credits.Ident
	wg        sync.WaitGroup
	port      uint16

	udp      *server.UDP
	udpAsked map[string]time.Time // server addr -> last UDP source request

	ctx          context.Context
	connectBacks atomic.Int32      // Kad firewall checks for other nodes running
	kadTasks     atomic.Int32      // our own Kad checks running (UDP test)
	buddyBusy    bool              // a connection to our Kad buddy is up or being made
	lastVerified map[ed2k.Hash]int // verified part counts, to refresh the server offer

	// connectBack* count the firewall checks for others started in the
	// current minute.
	connectBackMinute time.Time
	connectBackCount  int

	inbound   connLimiter  // incoming peer connections
	callbacks callbackGate // server callback requests we act on

	sx sxState // source exchange

	received atomic.Int64 // bytes of file data received from peers since start
	dlLimit  *netutil.Limiter

	// kadCalled is when we last asked each address (a buddy, or a type 6
	// source) for a Kad callback.
	kadCalled map[netip.Addr]time.Time

	searchMisses   int  // searches in a row the connected server left unanswered
	silentReported bool // OnSilentServer was called for the connected server

	msgMu      sync.Mutex // peer chat messages logged in the current minute
	msgWindow  time.Time
	msgLogged  int
	msgDropped int
}

// NewManager creates a manager. Add files, then call Run.
func NewManager(cfg Config) *Manager {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Debug == nil {
		cfg.Debug = func(string, ...any) {}
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 40
	}
	m := &Manager{cfg: cfg, files: map[ed2k.Hash]*File{}, conns: map[*peer.Conn]*File{}, idents: map[*peer.Conn]*credits.Ident{}, udpAsked: map[string]time.Time{}, lastVerified: map[ed2k.Hash]int{}, kadCalled: map[netip.Addr]time.Time{}}
	m.dlLimit = netutil.NewLimiter(cfg.DownloadLimitKBps * 1024)
	m.callbacks.allowPrivate = cfg.allowPrivateSources
	return m
}

// Add registers a part file for downloading.
func (m *Manager) Add(pf *PartFile) {
	prio := pf.Priority()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[pf.Hash]; !ok {
		m.files[pf.Hash] = &File{pf: pf, sources: map[string]*Source{}, speedTime: time.Now(), priority: prio}
		if m.cfg.Library != nil {
			m.cfg.Library.SetPartial(pf.Hash, sharedPart{pf})
		}
	}
}

// Run downloads until every file is complete or ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	stopLimit := context.AfterFunc(ctx, m.dlLimit.Stop)
	defer stopLimit()

	if m.cfg.Port != 0 {
		// IPv4 only: eD2K has no IPv6, and on IPv6 one customer's /64
		// would give unlimited addresses past the per-IP limit.
		ln, err := net.Listen("tcp4", net.JoinHostPort(m.cfg.ListenHost, strconv.Itoa(int(m.cfg.Port))))
		if err != nil {
			m.cfg.Log("cannot listen on port %d (%v); incoming connections disabled", m.cfg.Port, err)
		} else {
			m.mu.Lock()
			m.port = m.cfg.Port
			m.mu.Unlock()
			m.wg.Add(1)
			go m.acceptLoop(ctx, ln)
			go func() { <-ctx.Done(); ln.Close() }()
		}
	}

	m.wg.Add(1)
	go m.serverLoop(ctx)

	if m.cfg.Servers != nil {
		if u, err := server.ListenUDP(m.cfg.ListenHost); err != nil {
			m.cfg.Log("UDP disabled: %v", err)
		} else {
			m.udp = u
			u.OnSources = m.onUDPSources
			u.Log = m.cfg.Log
			go u.Run()
			go func() { <-ctx.Done(); u.Close() }()
			m.wg.Add(1)
			go m.udpLoop(ctx)
		}
	}
	if m.cfg.KadSources != nil {
		m.wg.Add(1)
		go m.kadLoop(ctx)
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	saveTick := time.NewTicker(30 * time.Second)
	defer saveTick.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return ctx.Err()
		case <-saveTick.C:
			m.saveAll()
			m.refreshShared()
		case <-tick.C:
			m.retryFinish()
			if m.allFinished() {
				cancel()
				m.shutdown()
				return nil
			}
			m.schedule(ctx)
		}
	}
}

func (m *Manager) allFinished() bool {
	if m.cfg.StayAlive {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.files {
		if !f.finished {
			return false
		}
	}
	return true
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	m.closing = true // connections accepted from now on are closed at once
	for c := range m.conns {
		c.Close()
	}
	if m.srv != nil {
		m.srv.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.files {
		if !f.finished {
			if err := f.pf.Close(); err != nil {
				m.cfg.Log("saving %s: %v", f.pf.Name, err)
			}
		}
	}
}

func (m *Manager) saveAll() {
	m.mu.Lock()
	files := make([]*File, 0, len(m.files))
	for _, f := range m.files {
		if !f.finished {
			files = append(files, f)
		}
	}
	m.mu.Unlock()
	for _, f := range files {
		if err := f.pf.Save(); err != nil {
			m.cfg.Log("saving %s: %v", f.pf.Name, err)
		}
	}
}

// ---- server connection ----

func (m *Manager) serverLoop(ctx context.Context) {
	defer m.wg.Done()
	for ctx.Err() == nil {
		scfg := server.Config{
			Port:       m.port,
			Crypt:      m.cfg.Obfuscation,
			OnCallback: func(ip net.IP, port uint16, h ed2k.Hash, crypt byte) { m.onCallback(ctx, ip, port, h, crypt) },
			Blocked:    m.cfg.IPFilter.BlockedIP,
		}
		m.mu.Lock()
		m.reconnect = false
		m.mu.Unlock()
		c, err := m.cfg.Connect(ctx, scfg)
		if err != nil {
			m.cfg.Log("server: %v; retrying in 30s", err)
			sleepCtx(ctx, 30*time.Second)
			continue
		}
		m.mu.Lock()
		m.srv = c
		m.searchMisses, m.silentReported = 0, false
		m.mu.Unlock()

		offered := -1
		offer := func() {
			if m.cfg.Library == nil {
				return
			}
			if v := m.cfg.Library.Version(); v != offered {
				offered = v
				m.offerFiles(c)
			}
		}
		offer()
		keep := time.NewTicker(10 * time.Minute)
		srcTick := time.NewTicker(2 * time.Second)
		offerTick := time.NewTicker(30 * time.Second)
		// Source requests run beside the loop, one at a time: a server
		// that does not answer one would otherwise hold up for its time
		// limit the keep-alives, file offers and a reconnection asked for.
		var srcBusy atomic.Bool
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-c.Done():
				m.cfg.Log("server connection lost: %v", c.Err())
				break loop
			case <-keep.C:
				if m.cfg.Library == nil {
					c.KeepAlive()
				} else {
					m.offerFiles(c) // a full offer also keeps the connection alive
				}
			case <-srcTick.C:
				if srcBusy.CompareAndSwap(false, true) {
					m.wg.Add(1)
					go func() {
						defer m.wg.Done()
						defer srcBusy.Store(false)
						safe.Catch(m.cfg.Log, "source request", func() { m.requestSources(ctx, c) }, m.locks()...)
					}()
				}
			case <-offerTick.C:
				offer()
			}
		}
		keep.Stop()
		srcTick.Stop()
		offerTick.Stop()
		c.Close()
		m.mu.Lock()
		m.srv = nil
		now := m.reconnect
		m.mu.Unlock()
		if !now {
			sleepCtx(ctx, 10*time.Second)
		}
	}
}

// Reconnect drops the current server connection and connects again at
// once; Config.Connect decides which server (e.g. the one just chosen).
func (m *Manager) Reconnect() {
	m.mu.Lock()
	c := m.srv
	m.reconnect = true
	m.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// requestSources asks the server for sources of the file that has waited
// longest, at most one request per call to avoid flooding the server.
func (m *Manager) requestSources(ctx context.Context, c *server.Conn) {
	m.mu.Lock()
	var pick *File
	for _, f := range m.files {
		if f.finished || f.paused || time.Since(f.lastSrcReq) < SourceReqInterval {
			continue
		}
		if pick == nil || f.lastSrcReq.Before(pick.lastSrcReq) {
			pick = f
		}
	}
	if pick != nil {
		pick.lastSrcReq = time.Now()
	}
	m.mu.Unlock()
	if pick == nil {
		return
	}

	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	srcs, err := c.GetSources(rctx, pick.pf.Hash, uint64(pick.pf.Size))
	if err != nil {
		m.cfg.Debug("%s: no answer to source request (%v)", pick.pf.Name, err)
		return
	}
	added := m.addSources(pick, srcs, c.Info().Addr(), c.Status().ClientID, true)
	m.cfg.Log("%s: server returned %d sources (%d new)", pick.pf.Name, len(srcs), added)
}

// addSources merges server-provided sources into f. LowID sources are
// only useful when they come from the server we are logged into (it
// relays callbacks), so allowLowID is false for other servers.
//
// Addresses come from servers and are not trusted: sources that are not
// public IPv4 addresses (our own machine or network) or have port 0 are
// dropped, at most MaxSourcesPerIP are kept per IP, and at most
// MaxSourcesPerFile in total, making room by forgetting dead sources.
func (m *Manager) addSources(f *File, srcs []server.Source, via string, ourID uint32, allowLowID bool) int {
	added := 0
	self := m.identIP() // also known without a server, from Kad
	m.mu.Lock()
	defer m.mu.Unlock()
	perIP := map[string]int{}
	lowIDs := 0
	for _, src := range f.sources {
		if src.IP != nil {
			perIP[src.IP.String()]++
		}
		if src.LowID != 0 {
			lowIDs++
		}
	}
	cur := ""
	if m.srv != nil {
		cur = m.srv.Info().Addr()
	}
	for _, s := range srcs {
		// Skip ourselves. Another machine behind the same router (e.g. a
		// NAS) has our public IP but a different port, so keep that one.
		if s.ID == ourID && (s.LowID() || s.Port == m.port) {
			continue
		}
		if m.isSelfLocked(s, self) {
			continue
		}
		if s.LowID() && !allowLowID {
			continue
		}
		src := &Source{Port: s.Port, Crypt: s.Crypt, UserHash: s.UserHash, file: f, NextAsk: time.Now()}
		if s.LowID() {
			src.LowID = s.ID
			src.server = via
			src.key = fmt.Sprintf("lowid:%d@%s", s.ID, src.server)
		} else {
			src.IP = server.IPFromU32(s.ID)
			if s.Port == 0 || (!netutil.IsPublicIPv4(src.IP) && !m.cfg.allowPrivateSources) || m.blocked(src.IP) {
				continue
			}
			src.key = net.JoinHostPort(src.IP.String(), strconv.Itoa(int(s.Port)))
		}
		if _, ok := f.sources[src.key]; ok {
			continue
		}
		if src.IP != nil && perIP[src.IP.String()] >= MaxSourcesPerIP {
			continue
		}
		if src.LowID != 0 && lowIDs >= MaxLowIDPerFile && !evictDeadLowID(f, cur) {
			continue
		}
		if len(f.sources) >= MaxSourcesPerFile && !evictDeadSource(f, cur) {
			break // full of sources that may still work
		}
		f.sources[src.key] = src
		if src.IP != nil {
			perIP[src.IP.String()]++
		}
		if src.LowID != 0 {
			lowIDs++
		}
		added++
	}
	return added
}

// evictDeadSource forgets one source of f that will not be asked again:
// one without the file, unreachable too many times, or LowID behind a
// server other than cur (the one we are connected to), which cannot relay
// a callback to it. It reports false if there is none. Callers hold m.mu.
func evictDeadSource(f *File, cur string) bool {
	for key, src := range f.sources {
		if src.busy {
			continue
		}
		if src.State == StateNoFile || (src.State == StateUnreachable && src.Fails >= maxSourceFails) ||
			(src.LowID != 0 && cur != "" && src.server != cur) {
			delete(f.sources, key)
			return true
		}
	}
	return false
}

// evictDeadLowID forgets one LowID source of f that cannot be reached
// (behind another server, or that never called back), to make room for a
// new LowID source. Callers hold m.mu.
func evictDeadLowID(f *File, cur string) bool {
	for key, src := range f.sources {
		if src.busy || src.LowID == 0 {
			continue
		}
		if (cur != "" && src.server != cur) || src.Fails > 0 || src.State == StateNoFile {
			delete(f.sources, key)
			return true
		}
	}
	return false
}

// ---- UDP source requests to every known server ----

// udpLoop asks one server at a time (rotating through the list) for
// sources of every active download. Each server is asked at most once
// per SourceReqInterval, which is what eMule does.
func (m *Manager) udpLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var files []server.FileReq
		m.mu.Lock()
		for _, f := range m.files {
			if !f.finished && !f.paused {
				files = append(files, server.FileReq{Hash: f.pf.Hash, Size: uint64(f.pf.Size)})
			}
		}
		connected := ""
		if m.srv != nil {
			connected = m.srv.Info().Addr()
		}
		m.mu.Unlock()
		if len(files) == 0 {
			continue
		}
		for _, s := range m.servers() {
			addr := s.Addr()
			if addr == connected || time.Since(m.udpAsked[addr]) < SourceReqInterval {
				continue // the connected server is asked over TCP
			}
			m.udpAsked[addr] = time.Now()
			if err := m.udp.GetSources(s, files); err != nil {
				m.cfg.Debug("UDP source request to %s: %v", addr, err)
			} else {
				m.cfg.Debug("UDP: asked %s for sources of %d files", addr, len(files))
			}
			break // one server per tick
		}
	}
}

func (m *Manager) onUDPSources(from *net.UDPAddr, h ed2k.Hash, srcs []server.Source) {
	m.mu.Lock()
	f := m.files[h]
	var our uint32
	if m.srv != nil {
		our = m.srv.Status().ClientID
	}
	m.mu.Unlock()
	if f == nil || len(srcs) == 0 {
		return
	}
	if added := m.addSources(f, srcs, server.ServerKey(from), our, false); added > 0 {
		m.cfg.Log("%s: %d new sources from %s (UDP)", f.pf.Name, added, server.ServerKey(from))
	}
}

// ---- Kad source searches ----

// Kad source searches, as eMule: each file once an hour, at most 5 at a
// time (a search lasts 45 seconds), one started per tick.
const (
	KadReaskInterval  = time.Hour
	kadMaxSearches    = 5
	kadSearchDuration = 45 * time.Second
)

func (m *Manager) kadLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		safe.Catch(m.cfg.Log, "kad sources", func() { m.kadTick(time.Now()) }, m.locks()...)
	}
}

// kadTick starts the Kad source search that is due, if any.
func (m *Manager) kadTick(now time.Time) {
	m.mu.Lock()
	running := 0
	var due *File
	for _, f := range m.files {
		if now.Sub(f.kadAsked) < kadSearchDuration {
			running++
		}
	}
	for _, f := range m.files {
		if f.finished || f.paused || len(f.sources) >= MaxSourcesPerFile || now.Sub(f.kadAsked) < KadReaskInterval {
			continue
		}
		if due == nil || f.kadAsked.Before(due.kadAsked) {
			due = f
		}
	}
	if due == nil || running >= kadMaxSearches {
		m.mu.Unlock()
		return
	}
	hash, size := due.pf.Hash, uint64(due.pf.Size)
	// Marked even if the search cannot start (Kad not connected yet):
	// tried again a minute later rather than every second.
	due.kadAsked = now.Add(-KadReaskInterval + time.Minute)
	m.mu.Unlock()
	err := m.cfg.KadSources(hash, size, func(s kad.Source) {
		m.onKadSource(hash, s)
	})
	if err == nil {
		m.mu.Lock()
		due.kadAsked = now
		m.mu.Unlock()
	}
}

func (m *Manager) onKadSource(h ed2k.Hash, s kad.Source) {
	m.mu.Lock()
	f := m.files[h]
	var our uint32
	if m.srv != nil {
		our = m.srv.Status().ClientID
	}
	m.mu.Unlock()
	if f == nil {
		return
	}
	if !s.Direct() {
		// Firewalled sources: through their buddy, whose address they must
		// give (without it every callback would be a lookup in Kad), or
		// by a UDP callback to them (type 6, with the option).
		typeOK := ((s.Type == 3 || s.Type == 5) && kad.GoodAddr(s.BuddyIP, s.BuddyPort) && !s.Buddy.IsZero()) ||
			(s.Type == 6 && s.Crypt&kad.ConnectDirectCallback != 0 && kad.GoodAddr(s.IP, s.UDPPort))
		if typeOK && m.cfg.KadCallback != nil && m.addKadCallbackSource(f, s) {
			m.cfg.Debug("%s: new firewalled source from Kad", f.pf.Name)
		}
		return
	}
	ip := net.IP(s.IP.AsSlice())
	src := server.Source{ID: server.IPToU32(ip), Port: s.TCPPort, Crypt: s.Crypt, UserHash: s.UserHash}
	if added := m.addSources(f, []server.Source{src}, "kad", our, false); added > 0 {
		m.cfg.Debug("%s: new source %s from Kad", f.pf.Name, src.Addr())
	}
}

// ---- scheduling ----

func (m *Manager) schedule(ctx context.Context) {
	now := time.Now()
	var dial []*Source
	var callbacks []uint32
	type kadCB struct {
		src  kad.Source
		file ed2k.Hash
	}
	var kadCallbacks []kadCB
	var udpAsks []udpAsk

	m.mu.Lock()
	srv := m.srv
	high := srv != nil && srv.Status().HighID
	for _, f := range m.byPriorityLocked() {
		if f.finished || f.paused {
			continue
		}
		for key, src := range f.sources {
			if src.busy || now.Before(src.NextAsk) || src.State == StateNoFile {
				continue
			}
			if src.State == StateUnreachable && src.Fails >= maxSourceFails {
				delete(f.sources, key)
				continue
			}
			if m.active >= m.cfg.MaxConns {
				break
			}
			if src.kadFW != nil {
				// Reached only when it connects to us: ask its buddy (or
				// it, over UDP); an answer that never came is a failure.
				if !src.callbackAt.IsZero() && now.Sub(src.callbackAt) > callbackWait {
					src.callbackAt = time.Time{}
					src.Fails++
					src.State = StateUnreachable
					if src.Fails >= maxSourceFails {
						delete(f.sources, key)
						continue
					}
				}
				// A few per second, and each address (a buddy may be
				// named by many forged sources) once a minute.
				if len(kadCallbacks) >= kadCallbacksPerSecond {
					continue
				}
				to := src.kadFW.BuddyIP
				if src.kadFW.Type == 6 {
					to = src.kadFW.IP
				}
				if at, ok := m.kadCalled[to]; ok && now.Sub(at) < kadCallEvery {
					src.NextAsk = at.Add(kadCallEvery)
					continue
				}
				m.kadCalled[to] = now
				kadCallbacks = append(kadCallbacks, kadCB{*src.kadFW, f.pf.Hash})
				src.callbackAt = now
				src.NextAsk = now.Add(ReaskInterval)
				continue
			}
			if src.LowID != 0 {
				// A callback we asked for and that never came counts as
				// a failed connection, so LowID sources that do not
				// exist (or never answer) are eventually forgotten.
				if !src.callbackAt.IsZero() && now.Sub(src.callbackAt) > callbackWait {
					src.callbackAt = time.Time{}
					src.Fails++
					src.State = StateUnreachable
					if src.Fails >= maxSourceFails {
						delete(f.sources, key)
						continue
					}
				}
				// A firewalled peer can only reach us if we are HighID
				// and share its server: ask the server to relay a callback.
				if high && src.server == srv.Info().Addr() {
					callbacks = append(callbacks, src.LowID)
					src.callbackAt = now
				}
				src.NextAsk = now.Add(ReaskInterval)
				continue
			}
			if !src.udpAskAt.IsZero() {
				// Asked by UDP, and no answer in time: TCP this time.
				src.udpAskAt = time.Time{}
				src.udpFails++
			} else if m.udpReaskable(src, high) {
				// The answers do not say which file they are for: one
				// question to an address at a time.
				if !m.udpPendingLocked(src, now) {
					udpAsks = append(udpAsks, m.udpAskLocked(src, now))
				}
				continue
			}
			src.busy = true
			src.State = StateConnecting
			m.active++
			dial = append(dial, src)
		}
	}
	if len(m.kadCalled) > 256 {
		for ip, at := range m.kadCalled {
			if now.Sub(at) >= kadCallEvery {
				delete(m.kadCalled, ip)
			}
		}
	}
	m.mu.Unlock()

	for _, id := range callbacks {
		srv.RequestCallback(id)
	}
	for _, a := range udpAsks {
		m.cfg.SendClientUDP(a.to, peer.OpReaskFilePing, a.payload, a.userHash)
	}
	for _, cb := range kadCallbacks {
		if err := m.cfg.KadCallback(cb.src, cb.file); err != nil {
			m.cfg.Debug("Kad callback: %v", err)
		}
	}
	for _, src := range dial {
		m.wg.Add(1)
		go m.dialSource(ctx, src)
	}
}

func (m *Manager) dialSource(ctx context.Context, src *Source) {
	defer m.wg.Done()
	m.mu.Lock()
	hash, crypt := src.UserHash, src.Crypt
	m.mu.Unlock()
	nc, err := m.dial(ctx, "tcp", src.key, hash, crypt)
	if err != nil {
		m.cfg.Debug("%s [%s]: connect failed: %v", src.file.pf.Name, src.key, err)
		m.mu.Lock()
		if errors.Is(err, errObfuscation) {
			src.Crypt = 0 // next time without, if we may
		}
		src.Fails++
		src.State = StateUnreachable
		src.NextAsk = time.Now().Add(time.Duration(src.Fails) * 5 * time.Minute)
		src.busy = false
		m.active--
		m.mu.Unlock()
		return
	}
	m.runSession(ctx, nc, true, src)
}

// ---- incoming connections ----

func (m *Manager) acceptLoop(ctx context.Context, ln net.Listener) {
	defer m.wg.Done()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		ip := remoteIP(nc)
		if m.blocked(net.ParseIP(ip)) {
			m.cfg.Debug("incoming connection from %s refused: IP filter", ip)
			nc.Close()
			continue
		}
		if !m.inbound.acquire(ip) {
			m.cfg.Debug("incoming connection from %s refused: too many connections", ip)
			nc.Close()
			continue
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.inbound.release(ip)
			stop := context.AfterFunc(ctx, func() { nc.Close() })
			c, err := obfus.Accept(nc, m.cfg.UserHash, m.cfg.Obfuscation&obfus.Requires == 0, m.cfg.Obfuscation&obfus.Supports != 0)
			stop()
			if err != nil {
				m.cfg.Debug("incoming connection from %s: %v", ip, err)
				nc.Close()
				return
			}
			if c.Obfuscated() {
				m.cfg.Debug("obfuscated connection from %s", ip)
			}
			m.runSession(ctx, c, false, nil)
		}()
	}
}

// claimSource finds the source a just-identified peer corresponds to and
// marks it busy. Prefers sources where we are queued (the peer is likely
// calling us back to start an upload).
//
// What a peer says about itself in its hello (user hash, client ID, port)
// is not proof of anything: anyone can copy another client's values. So
// a HighID source only matches a connection from its own IP address, and
// a LowID source (whose address we do not know) only while we are waiting
// for the callback we asked its server for, and only once per request.
// Otherwise a stranger could take over our sources, learn which files we
// download, and keep our connection slots busy.
func (m *Manager) claimSource(h peer.Hello, ip net.IP) *Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var best *Source
	for _, f := range m.files {
		if f.finished || f.paused {
			continue
		}
		for _, src := range f.sources {
			if src.busy {
				continue
			}
			if !incomingMatches(src, h, ip, now) {
				continue
			}
			if best == nil || (src.State == StateQueued && best.State != StateQueued) {
				best = src
			}
		}
	}
	if best != nil {
		best.busy = true
		best.callbackAt = time.Time{}
		m.active++
	}
	return best
}

// callbackWait is how long after asking a server to relay a callback we
// accept the LowID peer's connection as that source.
const callbackWait = 2 * time.Minute

// incomingMatches reports whether a connection from ip, whose peer sent
// hello h, may be the source src.
func incomingMatches(src *Source, h peer.Hello, ip net.IP, now time.Time) bool {
	if src.kadFW != nil {
		// Known by the user hash it published under: while we wait for
		// the callback we asked for, or when it queued us and our turn
		// came.
		if src.UserHash != h.UserHash {
			return false
		}
		return src.State == StateQueued || (!src.callbackAt.IsZero() && now.Sub(src.callbackAt) <= callbackWait)
	}
	if src.LowID == 0 {
		if ip == nil || !src.IP.Equal(ip) {
			return false
		}
		return src.Port == h.Port || (!src.UserHash.IsZero() && src.UserHash == h.UserHash)
	}
	if src.LowID != h.ClientID {
		return false
	}
	// A LowID uploader that queued us connects to us by itself when our
	// slot comes: known by the user hash it gave us before.
	if src.State == StateQueued && !src.UserHash.IsZero() && src.UserHash == h.UserHash {
		return true
	}
	if src.callbackAt.IsZero() || now.Sub(src.callbackAt) > callbackWait {
		return false
	}
	return src.UserHash.IsZero() || src.UserHash == h.UserHash
}

func (m *Manager) releaseSource(src *Source) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src.busy = false
	m.active--
}

func (m *Manager) trackConn(c *peer.Conn, add bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if add {
		m.conns[c] = nil
		if m.closing {
			// Accepted while shutting down, after shutdown closed the
			// others: it would keep shutdown waiting for its time limits.
			c.Close()
		}
	} else {
		delete(m.conns, c)
		delete(m.idents, c)
	}
}

// errBlocked is returned by dial for an address the IP filter blocks.
var errBlocked = errors.New("blocked by the IP filter")

// blocked reports whether the IP filter blocks ip.
func (m *Manager) blocked(ip net.IP) bool { return m.cfg.IPFilter.BlockedIP(ip) }

// servers returns the known servers the IP filter lets through.
func (m *Manager) servers() []server.Info {
	var out []server.Info
	for _, s := range m.cfg.Servers() {
		if !m.blocked(s.IP) {
			out = append(out, s)
		}
	}
	return out
}

// errObfuscation is returned by dial when an obfuscated connection could
// not be made.
var errObfuscation = errors.New("obfuscation")

// dial connects to a peer at addr, with obfuscation if the peer takes it
// (userHash and crypt known) and we offer it. With obfuscation required,
// a peer that does not take it is not dialed.
func (m *Manager) dial(ctx context.Context, network, addr string, userHash ed2k.Hash, crypt byte) (net.Conn, error) {
	if host, _, err := net.SplitHostPort(addr); err == nil && m.blocked(net.ParseIP(host)) {
		return nil, errBlocked
	}
	ours := m.cfg.Obfuscation
	obf := ours&obfus.Supports != 0 && crypt&obfus.Supports != 0 && !userHash.IsZero()
	if !obf && ours&obfus.Requires != 0 {
		return nil, fmt.Errorf("%w required, and the peer does not support it (or we do not know its user hash)", errObfuscation)
	}
	d := net.Dialer{Timeout: ConnectTimeout}
	nc, err := d.DialContext(ctx, network, addr)
	if err != nil || !obf {
		return nc, err
	}
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	c, err := obfus.Client(nc, userHash)
	stop()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("%w: %v", errObfuscation, err)
	}
	m.cfg.Debug("obfuscated connection to %s", addr)
	return c, nil
}

// startIdent begins the secure identification on a connection whose
// hello exchange is done.
func (m *Manager) startIdent(c *peer.Conn, h peer.Hello) {
	if m.cfg.Credits == nil {
		return
	}
	// Our address as this peer sees it, which signatures cover: our
	// public one, or for a peer on our own network, the one we have there.
	ourIP := m.identIP
	if ip := c.RemoteIP(); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		ourIP = c.LocalIP
	}
	id := m.cfg.Credits.NewIdent(h, c.RemoteIP(), ourIP, func(op byte, payload []byte) error {
		return c.Send(proto.ProtoEMule, op, payload)
	})
	m.mu.Lock()
	if _, ok := m.conns[c]; ok {
		m.idents[c] = id
	}
	m.mu.Unlock()
	id.Start()
}

// identPacket handles the secure identification packets; it reports
// whether p was one.
func (m *Manager) identPacket(c *peer.Conn, p *proto.Packet) (bool, error) {
	m.mu.Lock()
	id := m.idents[c]
	m.mu.Unlock()
	if id == nil {
		return false, nil
	}
	return id.Handle(p)
}

// isSelfLocked reports whether s is this client: our user hash, or our
// public address (self, from identIP) with our port.
func (m *Manager) isSelfLocked(s server.Source, self net.IP) bool {
	if !s.UserHash.IsZero() && s.UserHash == m.cfg.UserHash {
		return true
	}
	return !s.LowID() && self != nil && s.Port == m.port && server.IPFromU32(s.ID).Equal(self)
}

// identIP is our public address for version 2 signatures: as the server
// sees it (HighID), or as Kad nodes do; nil when unknown.
func (m *Manager) identIP() net.IP {
	if a := m.PublicIP(); a.Is4() {
		return net.IP(a.AsSlice())
	}
	if m.cfg.KadStatus != nil {
		if a := m.cfg.KadStatus().ExternalIP.Unmap(); a.Is4() && kad.GoodAddr(a, 1) {
			return net.IP(a.AsSlice())
		}
	}
	return nil
}

// bindConn records which file a connection is serving, so pausing or
// cancelling that file can close it.
func (m *Manager) bindConn(c *peer.Conn, f *File) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conns[c]; ok {
		m.conns[c] = f
	}
}

// closeFileConnsLocked closes every peer connection serving f.
func (m *Manager) closeFileConnsLocked(f *File) {
	for c, cf := range m.conns {
		if cf == f {
			c.Close()
		}
	}
}

func (m *Manager) ourHello() peer.Hello {
	m.mu.Lock()
	srv := m.srv
	m.mu.Unlock()
	var id uint32
	var sip net.IP
	var sport uint16
	if srv != nil {
		id = srv.Status().ClientID
		sip, sport = srv.Info().IP, srv.Info().Port
	}
	h := peer.NewHello(m.cfg.UserHash, m.cfg.Nick, id, m.port, sip, sport).WithCrypt(m.cfg.Obfuscation)
	if m.cfg.Kad != nil {
		if port, version := m.cfg.Kad(); port != 0 {
			h = h.WithKad(port, version)
			if m.cfg.SendClientUDP != nil {
				h = h.WithClientUDP(port) // the Kad port is our client UDP port too
			}
			if m.cfg.KadFirewall != nil {
				fw := m.cfg.KadFirewall()
				h = h.WithKadFirewall(fw.BuddyIP, fw.BuddyPort, fw.DirectCallback)
			}
		}
	}
	return h
}

// kadPacket handles the Kad packets a peer sends over TCP; it reports
// whether p was one.
func (m *Manager) kadPacket(c *peer.Conn, p *proto.Packet) bool {
	if p.Proto != proto.ProtoEMule {
		return false
	}
	switch p.Opcode {
	case peer.OpKadFWTCPCheckAck:
		if m.cfg.KadFirewallAck != nil {
			m.cfg.KadFirewallAck(c.RemoteIP())
		}
	case kad.OpFWCheckUDPReq:
		if m.cfg.KadUDPCheckRequest != nil {
			m.cfg.KadUDPCheckRequest(c.RemoteIP(), p.Payload)
		}
	default:
		return false
	}
	return true
}

// maxConnectBacks is how many Kad firewall checks for other nodes run at
// once, and maxConnectBacksPerMin how many start per minute (the requests
// may come from forged addresses, and each costs a connection); our own
// checks (kadTask) have a budget of their own, so such requests cannot
// keep our UDP test from running.
const (
	maxConnectBacks       = 4
	maxConnectBacksPerMin = 20
)

// KadConnectBack connects to a Kad node that asked us to check whether
// its TCP port is reachable, and tells it we got through. userHash, if not
// zero, is the user hash the node gave: a peer answering with another one
// is not that node. It returns false when it will not try (too many checks
// running, our own address, or shutting down).
func (m *Manager) KadConnectBack(ip net.IP, port uint16, userHash ed2k.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if port == 0 || (port == m.port && userHash == m.cfg.UserHash) {
		return false
	}
	ctx := m.ctx
	if ctx == nil || ctx.Err() != nil || m.closing {
		return false
	}
	now := time.Now()
	if now.Sub(m.connectBackMinute) >= time.Minute {
		m.connectBackMinute, m.connectBackCount = now, 0
	}
	if m.connectBackCount >= maxConnectBacksPerMin {
		return false
	}
	if m.connectBacks.Add(1) > maxConnectBacks {
		m.connectBacks.Add(-1)
		return false
	}
	m.connectBackCount++
	// Added under m.mu with closing false: shutdown sets closing under
	// m.mu before it waits, so it waits for this one too.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.connectBacks.Add(-1)
		safe.Catch(m.cfg.Log, "kad connect back", func() { m.connectBack(ctx, ip, port, userHash) }, m.locks()...)
	}()
	return true
}

func (m *Manager) connectBack(ctx context.Context, ip net.IP, port uint16, userHash ed2k.Hash) {
	nc, err := m.dial(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), ed2k.Hash{}, 0)
	if err != nil {
		m.cfg.Debug("kad firewall check for %s:%d: cannot connect: %v", ip, port, err)
		return
	}
	c := peer.NewConn(nc)
	m.trackConn(c, true)
	defer m.trackConn(c, false)
	defer c.Close()
	h, err := m.handshake(c, true)
	if err != nil {
		m.cfg.Debug("kad firewall check for %s:%d: %v", ip, port, err)
		return
	}
	if !userHash.IsZero() && h.UserHash != userHash {
		m.cfg.Debug("kad firewall check for %s:%d: another client answered", ip, port)
		return
	}
	switch {
	case h.KadVersion() >= 7:
		c.Send(proto.ProtoEMule, peer.OpKadFWTCPCheckAck, nil)
		// Let it arrive before closing.
		c.Read(5 * time.Second)
	case h.KadPort() != 0 && m.cfg.KadLegacyAck != nil:
		m.cfg.KadLegacyAck(ip, h.KadPort())
	}
}

// finishRetry is how often finishing a complete download is tried again
// after it failed (disk full, folder missing...).
const finishRetry = time.Minute

// retryFinish finishes downloads that are complete but were never moved:
// the final step failed, or MyMule stopped before or during it. On the
// first tick after start this also picks up downloads that completed in
// a previous run.
func (m *Manager) retryFinish() {
	var due []*File
	m.mu.Lock()
	for _, f := range m.files {
		if !f.finished && !f.finishing && !f.paused && time.Since(f.finishTry) >= finishRetry {
			due = append(due, f)
		}
	}
	m.mu.Unlock()
	for _, f := range due {
		if f.pf.Complete() {
			m.goFinish(f)
		}
	}
}

// maxPeerMessagesPerMinute caps how many chat messages from peers are
// written to the log: anyone can send them, and without a cap a flood of
// them could fill the disk the log is on.
const maxPeerMessagesPerMinute = 20

// logPeerMessage logs a chat message from a peer, within the cap.
func (m *Manager) logPeerMessage(from, text string) {
	m.msgMu.Lock()
	now := time.Now()
	dropped := 0
	if now.Sub(m.msgWindow) >= time.Minute {
		dropped = m.msgDropped
		m.msgWindow, m.msgLogged, m.msgDropped = now, 0, 0
	}
	if m.msgLogged >= maxPeerMessagesPerMinute {
		m.msgDropped++
		m.msgMu.Unlock()
		return
	}
	m.msgLogged++
	m.msgMu.Unlock()
	if dropped > 0 {
		m.cfg.Log("%d more messages from peers were not logged (too many)", dropped)
	}
	m.cfg.Log("message from %s: %q", from, text)
}

// testSessionHook, when set by a test, runs at the start of every peer
// session (to check that a panic there is contained).
var testSessionHook func()

// locks lists the locks a recovered panic must not leave held.
func (m *Manager) locks() []safe.TryLocker {
	l := []safe.TryLocker{&m.mu}
	if m.cfg.Upload != nil {
		l = append(l, m.cfg.Upload)
	}
	return l
}

// goFinish runs finish in the background, counted in m.wg: stopping
// waits for it, instead of exiting in the middle of the final move and
// leaving an empty placeholder or a half-copied file behind.
func (m *Manager) goFinish(f *File) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.finish(f)
	}()
}

// finish moves a completed file to the incoming directory (once).
func (m *Manager) finish(f *File) {
	defer safe.Recover(m.cfg.Log, "finishing "+f.pf.Name, m.locks()...)
	m.mu.Lock()
	if f.finishing || f.finished {
		m.mu.Unlock()
		return
	}
	f.finishing = true
	f.finishTry = time.Now()
	m.mu.Unlock()

	path, err := f.pf.Finish(m.cfg.IncomingDir)
	m.mu.Lock()
	if err != nil {
		f.finishing = false
		told := f.finishFailed
		f.finishFailed = true
		m.mu.Unlock()
		m.cfg.Log("%s: cannot finish: %v", f.pf.Name, err)
		if !told && m.cfg.OnError != nil {
			m.cfg.OnError(f.pf.Name, err)
		}
		return
	}
	f.finished = true
	f.path = path
	m.mu.Unlock()
	if lib := m.cfg.Library; lib != nil {
		lib.SetPartial(f.pf.Hash, nil)
		lib.AddComplete(path, f.pf.Hash, f.pf.HashSet())
	}
	if m.cfg.OnComplete != nil {
		m.cfg.OnComplete(f.pf.Hash, f.pf.Name, path)
	}
}

// ---- status ----

// FileStatus is a snapshot of one download.
type FileStatus struct {
	Hash        ed2k.Hash
	Name        string
	Size, Done  int64
	Speed       float64 // bytes/s
	Sources     int
	Queued      int
	Downloading int
	BestRank    int
	Corrupt     int
	Finished    bool
	Paused      bool
	Path        string
	Priority    int  // Low, Normal or High
	Preview     bool // first and last parts first
}

// ServerStatus describes the current server connection.
type ServerStatus struct {
	Connected bool
	Name      string
	Addr      string
	HighID    bool
	Port      uint16
}

// Status returns a snapshot of every download, updating speed estimates.
func (m *Manager) Status() (ServerStatus, []FileStatus) {
	m.mu.Lock()
	var ss ServerStatus
	ss.Port = m.port
	if m.srv != nil {
		st := m.srv.Status()
		ss = ServerStatus{Connected: true, Name: m.srv.Info().Name, Addr: m.srv.Info().Addr(), HighID: st.HighID, Port: m.port}
	}
	now := time.Now()
	var out []FileStatus
	var pfs []*PartFile
	for _, f := range m.files {
		fs := FileStatus{Hash: f.pf.Hash, Name: f.pf.Name, Size: f.pf.Size, Finished: f.finished, Paused: f.paused, Path: f.path,
			Priority: f.priority}
		if dt := now.Sub(f.speedTime).Seconds(); dt >= 1 {
			b := f.bytes.Load()
			inst := float64(b-f.speedBytes) / dt
			f.speed = 0.6*inst + 0.4*f.speed // smooth
			f.speedBytes, f.speedTime = b, now
		}
		fs.Speed = f.speed
		for _, s := range f.sources {
			if s.State == StateNoFile {
				continue
			}
			fs.Sources++
			switch s.State {
			case StateQueued:
				fs.Queued++
				if s.Rank > 0 && (fs.BestRank == 0 || s.Rank < fs.BestRank) {
					fs.BestRank = s.Rank
				}
			case StateDownloading:
				fs.Downloading++
			}
		}
		out = append(out, fs)
		pfs = append(pfs, f.pf)
	}
	m.mu.Unlock()
	// Each download's own state is read without m.mu: a download busy
	// hashing must not hold up the whole manager.
	for i, pf := range pfs {
		out[i].Corrupt = pf.Corrupt()
		out[i].Preview = pf.Preview()
		if out[i].Finished {
			out[i].Done = out[i].Size
		} else {
			out[i].Done, _ = pf.Progress()
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ss, out
}

// ---- sharing ----

// offerFiles announces shared files to the server. Partial downloads are
// only announced once they have at least one verified part.
func (m *Manager) offerFiles(c *server.Conn) {
	var offers []server.Offer
	for _, f := range m.cfg.Library.List() {
		if f.Partial() {
			n := 0
			for _, a := range f.Avail() {
				if a {
					n++
				}
			}
			if n == 0 {
				continue
			}
		}
		offers = append(offers, server.Offer{
			Hash: f.FileHash(), Name: f.FileName(), Size: uint64(f.FileSize()),
			Type: server.FileTypeFor(f.FileName()), Complete: !f.Partial(),
		})
	}
	if err := c.OfferFiles(offers); err != nil {
		m.cfg.Debug("offering files: %v", err)
		return
	}
	m.cfg.Debug("offered %d shared files to the server", len(offers))
}

// refreshShared bumps the library version when a partial download gets
// its first verified part, so the server offer includes it.
func (m *Manager) refreshShared() {
	lib := m.cfg.Library
	if lib == nil {
		return
	}
	changed := false
	m.mu.Lock()
	active := map[ed2k.Hash]*PartFile{}
	for h, f := range m.files {
		if !f.finished {
			active[h] = f.pf
		}
	}
	m.mu.Unlock()
	counts := map[ed2k.Hash]int{}
	for h, pf := range active { // without m.mu (see Status)
		counts[h] = pf.VerifiedCount()
	}
	m.mu.Lock()
	for h, n := range counts {
		if old, ok := m.lastVerified[h]; ok && (old == 0) != (n == 0) {
			changed = true
		}
		m.lastVerified[h] = n
	}
	m.mu.Unlock()
	if changed {
		lib.Touch()
	}
}

// DialForUpload connects to a queued peer whose upload turn has come. It
// is the upload manager's Dial function.
func (m *Manager) DialForUpload(ip net.IP, port uint16, userHash ed2k.Hash, crypt byte) {
	m.mu.Lock()
	ctx := m.ctx
	if ctx == nil || ctx.Err() != nil || m.closing {
		m.mu.Unlock()
		return
	}
	// Added under m.mu before shutdown can start waiting.
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()
	nc, err := m.dial(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), userHash, crypt)
	if err != nil {
		m.cfg.Debug("upload: cannot reach %s:%d: %v", ip, port, err)
		return
	}
	m.runSession(ctx, nc, true, nil)
}
