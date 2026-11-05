package download

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/peer"
	"mymule/internal/server"
	"mymule/internal/share"
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
	// Connect logs into some server with the given config (the manager
	// fills in callbacks). Called again whenever the connection drops.
	Connect func(ctx context.Context, cfg server.Config) (*server.Conn, error)
	// Servers returns the known server list. If set, every server in it
	// is asked for sources over UDP (without logging in), in rotation.
	Servers func() []server.Info
	// Library and Upload enable sharing: partial and finished downloads
	// are shared, offered to the server and served to peers.
	Library    *share.Library
	Upload     *upload.Manager
	Log        func(format string, args ...any) // important events
	Debug      func(format string, args ...any) // per-peer chatter
	Trace      bool                             // log every peer packet via Debug
	OnComplete func(hash ed2k.Hash, name, path string)
}

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

	State   SourceState
	Rank    int
	Avail   []bool // nil = unknown or complete file
	NextAsk time.Time
	Fails   int
	Bytes   int64

	busy bool
	file *File
}

// File is a download managed by the Manager.
type File struct {
	pf         *PartFile
	sources    map[string]*Source
	lastSrcReq time.Time
	bytes      atomic.Int64
	finishing  bool
	finished   bool
	paused     bool
	path       string

	speedBytes int64
	speedTime  time.Time
	speed      float64
}

// Manager runs downloads: it keeps a server connection, collects sources,
// schedules peer connections and accepts incoming ones.
type Manager struct {
	cfg Config

	mu     sync.Mutex
	files  map[ed2k.Hash]*File
	srv    *server.Conn
	active int
	conns  map[*peer.Conn]*File // live peer connections, and the file they serve (once known)
	wg     sync.WaitGroup
	port   uint16

	udp      *server.UDP
	udpAsked map[string]time.Time // server addr -> last UDP source request

	ctx          context.Context
	lastVerified map[ed2k.Hash]int // verified part counts, to refresh the server offer
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
	return &Manager{cfg: cfg, files: map[ed2k.Hash]*File{}, conns: map[*peer.Conn]*File{}, udpAsked: map[string]time.Time{}, lastVerified: map[ed2k.Hash]int{}}
}

// Add registers a part file for downloading.
func (m *Manager) Add(pf *PartFile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[pf.Hash]; !ok {
		m.files[pf.Hash] = &File{pf: pf, sources: map[string]*Source{}, speedTime: time.Now()}
		if m.cfg.Library != nil {
			m.cfg.Library.SetPartial(pf.Hash, sharedPart{pf})
		}
	}
}

// Run downloads until every file is complete or ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.ctx = ctx

	if m.cfg.Port != 0 {
		ln, err := net.Listen("tcp", net.JoinHostPort(m.cfg.ListenHost, strconv.Itoa(int(m.cfg.Port))))
		if err != nil {
			m.cfg.Log("cannot listen on port %d (%v); incoming connections disabled", m.cfg.Port, err)
		} else {
			m.port = m.cfg.Port
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
			go u.Run()
			go func() { <-ctx.Done(); u.Close() }()
			m.wg.Add(1)
			go m.udpLoop(ctx)
		}
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
			f.pf.Close()
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
			OnCallback: func(ip net.IP, port uint16) { m.onCallback(ctx, ip, port) },
		}
		c, err := m.cfg.Connect(ctx, scfg)
		if err != nil {
			m.cfg.Log("server: %v; retrying in 30s", err)
			sleepCtx(ctx, 30*time.Second)
			continue
		}
		m.mu.Lock()
		m.srv = c
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
				m.requestSources(ctx, c)
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
		m.mu.Unlock()
		sleepCtx(ctx, 10*time.Second)
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
func (m *Manager) addSources(f *File, srcs []server.Source, via string, ourID uint32, allowLowID bool) int {
	added := 0
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range srcs {
		// Skip ourselves. Another machine behind the same router (e.g. a
		// NAS) has our public IP but a different port, so keep that one.
		if s.ID == ourID && (s.LowID() || s.Port == m.port) {
			continue
		}
		if s.LowID() && !allowLowID {
			continue
		}
		src := &Source{Port: s.Port, file: f, NextAsk: time.Now()}
		if s.LowID() {
			src.LowID = s.ID
			src.server = via
			src.key = fmt.Sprintf("lowid:%d@%s", s.ID, src.server)
		} else {
			src.IP = server.IPFromU32(s.ID)
			src.key = net.JoinHostPort(src.IP.String(), strconv.Itoa(int(s.Port)))
		}
		if _, ok := f.sources[src.key]; !ok {
			f.sources[src.key] = src
			added++
		}
	}
	return added
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
		for _, s := range m.cfg.Servers() {
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

// ---- scheduling ----

func (m *Manager) schedule(ctx context.Context) {
	now := time.Now()
	var dial []*Source
	var callbacks []uint32

	m.mu.Lock()
	srv := m.srv
	high := srv != nil && srv.Status().HighID
	for _, f := range m.files {
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
			if src.LowID != 0 {
				// A firewalled peer can only reach us if we are HighID
				// and share its server: ask the server to relay a callback.
				if high && src.server == srv.Info().Addr() {
					callbacks = append(callbacks, src.LowID)
				}
				src.NextAsk = now.Add(ReaskInterval)
				continue
			}
			src.busy = true
			src.State = StateConnecting
			m.active++
			dial = append(dial, src)
		}
	}
	m.mu.Unlock()

	for _, id := range callbacks {
		srv.RequestCallback(id)
	}
	for _, src := range dial {
		m.wg.Add(1)
		go m.dialSource(ctx, src)
	}
}

func (m *Manager) dialSource(ctx context.Context, src *Source) {
	defer m.wg.Done()
	d := net.Dialer{Timeout: ConnectTimeout}
	nc, err := d.DialContext(ctx, "tcp", src.key)
	if err != nil {
		m.cfg.Debug("%s [%s]: connect failed: %v", src.file.pf.Name, src.key, err)
		m.mu.Lock()
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
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.runSession(ctx, nc, false, nil)
		}()
	}
}

// onCallback handles a server-relayed request from a LowID peer that
// wants us to connect to it (it is probably uploading to us).
func (m *Manager) onCallback(ctx context.Context, ip net.IP, port uint16) {
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
	d := net.Dialer{Timeout: ConnectTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return
	}
	m.wg.Add(1)
	defer m.wg.Done()
	m.runSession(ctx, nc, true, nil)
}

// claimSource finds the source a just-identified peer corresponds to and
// marks it busy. Prefers sources where we are queued (the peer is likely
// calling us back to start an upload).
func (m *Manager) claimSource(h peer.Hello, ip net.IP) *Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Source
	for _, f := range m.files {
		if f.finished || f.paused {
			continue
		}
		for _, src := range f.sources {
			if src.busy {
				continue
			}
			match := (src.LowID == 0 && src.IP.Equal(ip) && src.Port == h.Port) ||
				(!src.UserHash.IsZero() && src.UserHash == h.UserHash) ||
				(src.LowID != 0 && src.LowID == h.ClientID)
			if !match {
				continue
			}
			if best == nil || (src.State == StateQueued && best.State != StateQueued) {
				best = src
			}
		}
	}
	if best != nil {
		best.busy = true
		m.active++
	}
	return best
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
	} else {
		delete(m.conns, c)
	}
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
	return peer.NewHello(m.cfg.UserHash, m.cfg.Nick, id, m.port, sip, sport)
}

// finish moves a completed file to the incoming directory (once).
func (m *Manager) finish(f *File) {
	m.mu.Lock()
	if f.finishing {
		m.mu.Unlock()
		return
	}
	f.finishing = true
	m.mu.Unlock()

	path, err := f.pf.Finish(m.cfg.IncomingDir)
	m.mu.Lock()
	if err != nil {
		f.finishing = false
		m.mu.Unlock()
		m.cfg.Log("%s: cannot finish: %v", f.pf.Name, err)
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
	defer m.mu.Unlock()
	var ss ServerStatus
	ss.Port = m.port
	if m.srv != nil {
		st := m.srv.Status()
		ss = ServerStatus{Connected: true, Name: m.srv.Info().Name, Addr: m.srv.Info().Addr(), HighID: st.HighID, Port: m.port}
	}
	now := time.Now()
	var out []FileStatus
	for _, f := range m.files {
		fs := FileStatus{Hash: f.pf.Hash, Name: f.pf.Name, Size: f.pf.Size, Finished: f.finished, Paused: f.paused, Path: f.path}
		fs.Corrupt = f.pf.Corrupt()
		if f.finished {
			fs.Done = fs.Size
		} else {
			fs.Done, _ = f.pf.Progress()
		}
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
	for h, f := range m.files {
		if f.finished {
			continue
		}
		n := f.pf.VerifiedCount()
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
func (m *Manager) DialForUpload(ip net.IP, port uint16) {
	ctx := m.ctx
	if ctx == nil || ctx.Err() != nil {
		return
	}
	d := net.Dialer{Timeout: ConnectTimeout}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
	if err != nil {
		m.cfg.Debug("upload: cannot reach %s:%d: %v", ip, port, err)
		return
	}
	m.wg.Add(1)
	defer m.wg.Done()
	m.runSession(ctx, nc, true, nil)
}
