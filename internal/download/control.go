package download

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/kad"
	"mymule/internal/safe"
	"mymule/internal/server"
	"mymule/internal/share"
	"mymule/internal/upload"
)

// Runtime control used by the daemon API: adding, pausing, cancelling
// downloads and searching through the manager's server connection.

var (
	ErrExists      = errors.New("already downloading this file")
	ErrNotFound    = errors.New("no download matches")
	ErrNoServer    = errors.New("not connected to a server")
	ErrAmbiguous   = errors.New("several downloads match; be more specific")
	ErrNoTempDir   = errors.New("manager has no temp dir configured")
	ErrBadSource   = errors.New("invalid source address (want ip:port)")
	ErrFinishedYet = errors.New("download already finished")
)

// AddLink starts downloading link. Optional sources ("ip:port") are added
// right away, e.g. a machine on the local network known to have the file.
func (m *Manager) AddLink(link ed2k.Link, sources ...string) error {
	if m.cfg.TempDir == "" {
		return ErrNoTempDir
	}
	m.mu.Lock()
	_, exists := m.files[link.Hash]
	m.mu.Unlock()
	if !exists {
		pf, err := create(m.cfg.TempDir, link, !m.cfg.SkipFreeSpaceCheck)
		if err != nil {
			return err
		}
		m.Add(pf)
		m.cfg.Log("added %s", link.Name)
	} else if len(sources) == 0 {
		return ErrExists
	}
	for _, s := range sources {
		if err := m.AddSource(link.Hash, s); err != nil {
			return err
		}
	}
	return nil
}

// AddSource adds a peer address as a source for a download.
func (m *Manager) AddSource(h ed2k.Hash, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return ErrBadSource
	}
	ip := net.ParseIP(host).To4()
	port, err := strconv.ParseUint(portStr, 10, 16)
	if ip == nil || err != nil || port == 0 {
		return ErrBadSource
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[h]
	if f == nil {
		return ErrNotFound
	}
	key := net.JoinHostPort(ip.String(), portStr)
	if _, ok := f.sources[key]; !ok {
		f.sources[key] = &Source{key: key, IP: ip, Port: uint16(port), file: f, NextAsk: time.Now()}
	}
	return nil
}

// Find resolves a user query (hash prefix or part of the name) to exactly
// one download.
func (m *Manager) Find(query string) (ed2k.Hash, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return ed2k.Hash{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var matches []ed2k.Hash
	for h, f := range m.files {
		if strings.HasPrefix(strings.ToLower(h.String()), q) {
			return h, nil // hash prefixes win outright
		}
		if strings.Contains(strings.ToLower(f.pf.Name), q) {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 0:
		return ed2k.Hash{}, fmt.Errorf("%w %q", ErrNotFound, query)
	case 1:
		return matches[0], nil
	}
	return ed2k.Hash{}, fmt.Errorf("%w (%d match %q)", ErrAmbiguous, len(matches), query)
}

// SetPaused pauses or resumes a download. Pausing drops its peer
// connections; queue positions are lost, as in eMule.
func (m *Manager) SetPaused(h ed2k.Hash, paused bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[h]
	if f == nil {
		return "", ErrNotFound
	}
	if f.finished {
		return f.pf.Name, ErrFinishedYet
	}
	f.paused = paused
	if paused {
		m.closeFileConnsLocked(f)
		go f.pf.Save()
	} else {
		now := time.Now()
		for _, s := range f.sources {
			if s.State != StateNoFile {
				s.NextAsk = now
			}
		}
		f.lastSrcReq = time.Time{}
	}
	return f.pf.Name, nil
}

// Cancel stops a download and deletes its data. Finished downloads are
// only removed from the list (the completed file is kept).
func (m *Manager) Cancel(h ed2k.Hash) (string, error) {
	m.mu.Lock()
	f := m.files[h]
	if f == nil {
		m.mu.Unlock()
		return "", ErrNotFound
	}
	delete(m.files, h)
	finished := f.finished
	m.closeFileConnsLocked(f)
	m.mu.Unlock()
	if m.cfg.Library != nil && !finished {
		m.cfg.Library.SetPartial(h, nil)
	}
	if finished {
		return f.pf.Name, nil
	}
	return f.pf.Name, f.pf.Delete()
}

// SetFinishedPath records that a completed file was moved (e.g. renamed
// into a movie library), so the status shows where it is now.
func (m *Manager) SetFinishedPath(h ed2k.Hash, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.files[h]; ok && f.finished {
		f.path = path
	}
}

// ClearFinished removes completed downloads from the list.
func (m *Manager) ClearFinished() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for h, f := range m.files {
		if f.finished {
			delete(m.files, h)
			n++
		}
	}
	return n
}

// Search runs a keyword search on the current server.
func (m *Manager) Search(ctx context.Context, q server.Query, morePages int) ([]server.Result, error) {
	m.mu.Lock()
	c := m.srv
	m.mu.Unlock()
	if c == nil {
		return nil, ErrNoServer
	}
	results, more, err := c.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := 0; i < morePages && more; i++ {
		var page []server.Result
		page, more, err = c.QueryMore(ctx)
		if err != nil {
			break
		}
		results = append(results, page...)
	}
	return results, nil
}

// Limits for merging global search results. Results come from servers
// (and before, from anyone who knew our UDP port); without them a single
// server could repeat a fake result to inflate its source count, or claim
// billions of sources, and rank it first - for the user and for the *arr
// apps that pick automatically.
const (
	maxReportedSources = 50000 // what one server may claim for one file
	maxGlobalResults   = 2000  // distinct files kept from one search
)

// connectedGrace is how long a global search still waits for the
// connected server once the other servers have had their time (a
// variable so tests can shorten it).
var connectedGrace = 3 * time.Second

// silentAfter is how many searches in a row the connected server may
// leave unanswered before Config.OnSilentServer is told.
const silentAfter = 3

// SearchGlobal runs a search on the connected server (TCP) and on every
// known server over UDP, merging results by hash. Source counts from
// different servers are added up, as eMule does for global searches; each
// server counts once per file, whatever it repeats.
//
// Both run at the same time. The connected server used to be asked first
// and alone: when it stopped answering searches (servers do, under load
// or to limit busy clients) the whole time went on waiting for it and the
// other servers were never asked, so the search came back empty. Now,
// once the other servers have had their time, the connected one gets
// connectedGrace more at most.
// kadSearchWait is how long a global search waits for Kad results: most
// arrive within seconds, and the rest of a Kad search (up to 45 s) adds
// little.
const kadSearchWait = 15 * time.Second

func (m *Manager) SearchGlobal(ctx context.Context, q server.Query, wait time.Duration) ([]server.Result, int, error) {
	var mg resultMerger

	m.mu.Lock()
	c := m.srv
	m.mu.Unlock()
	useUDP := m.udp != nil && m.cfg.Servers != nil
	if c == nil && !useUDP && m.cfg.KadSearch == nil {
		return nil, 0, ErrNoServer
	}

	// Kad, alongside the servers.
	kadDone := make(chan struct{})
	if m.cfg.KadSearch != nil {
		kctx, cancelKad := context.WithTimeout(ctx, kadSearchWait)
		defer cancelKad()
		go func() {
			defer close(kadDone)
			safe.Catch(m.cfg.Log, "kad search", func() {
				m.cfg.KadSearch(kctx, q, func(r server.Result) {
					// From other users, through Kad: an absurd size would
					// make a download allocate too much.
					if r.Size <= uint64(ed2k.MaxFileSize) && ed2k.ValidSize(int64(r.Size)) && r.Name != "" {
						mg.add("kad", r)
					}
				})
			})
		}()
	} else {
		close(kadDone)
	}
	defer func() { <-kadDone }()

	tcpDone := make(chan struct{})
	tctx, cancelTCP := context.WithCancel(ctx)
	defer cancelTCP()
	answered := false
	if c != nil {
		go func() {
			defer close(tcpDone)
			if res, err := m.Search(tctx, q, 0); err == nil {
				answered = true
				for _, r := range res {
					mg.add(c.Info().IP.String(), r)
				}
			}
		}()
	} else {
		close(tcpDone)
	}

	if useUDP {
		var others []server.Info
		for _, s := range m.cfg.Servers() {
			if c == nil || s.Addr() != c.Info().Addr() {
				others = append(others, s)
			}
		}
		m.udp.Search(ctx, others, q, wait, func(from *net.UDPAddr, r server.Result) {
			// By IP: some servers answer from more than one port.
			mg.add(from.IP.String(), r)
		})
		select {
		case <-tcpDone:
		case <-time.After(connectedGrace):
		case <-ctx.Done():
		}
		cancelTCP()
	}
	<-tcpDone // returns at once after cancelTCP; without UDP, waits as before
	if c != nil && ctx.Err() == nil {
		m.noteSearchAnswer(c, answered)
	}
	<-kadDone

	out, nServers := mg.results()
	return out, nServers, nil
}

// noteSearchAnswer counts searches the connected server c left without
// answer, and reports it once when it reaches silentAfter in a row.
func (m *Manager) noteSearchAnswer(c *server.Conn, answered bool) {
	m.mu.Lock()
	if m.srv != c {
		m.mu.Unlock()
		return
	}
	if answered {
		m.searchMisses = 0
		m.mu.Unlock()
		return
	}
	m.searchMisses++
	report := m.searchMisses >= silentAfter && !m.silentReported
	if report {
		m.silentReported = true
	}
	m.mu.Unlock()
	if report && m.cfg.OnSilentServer != nil {
		go m.cfg.OnSilentServer(c.Info().Addr())
	}
}

// resultMerger merges search results from several servers (see the
// limits above). It is safe for concurrent use.
type resultMerger struct {
	mu      sync.Mutex
	byHash  map[ed2k.Hash]*server.Result
	order   []ed2k.Hash
	servers map[string]bool
	counted map[string]bool // server IP + hash already added
}

// add merges result r reported by the server at fromIP.
func (mg *resultMerger) add(fromIP string, r server.Result) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if mg.byHash == nil {
		mg.byHash, mg.servers, mg.counted = map[ed2k.Hash]*server.Result{}, map[string]bool{}, map[string]bool{}
	}
	mg.servers[fromIP] = true
	key := fromIP + "|" + r.Hash.String()
	if mg.counted[key] {
		return
	}
	r.Sources = min(r.Sources, maxReportedSources)
	r.CompleteSources = min(r.CompleteSources, r.Sources)
	if have, ok := mg.byHash[r.Hash]; ok {
		mg.counted[key] = true
		have.Sources = satAdd(have.Sources, r.Sources)
		have.CompleteSources = satAdd(have.CompleteSources, r.CompleteSources)
		return
	}
	if len(mg.order) >= maxGlobalResults {
		return
	}
	mg.counted[key] = true
	cp := r
	mg.byHash[r.Hash] = &cp
	mg.order = append(mg.order, r.Hash)
}

// results returns the merged results in arrival order and how many
// servers answered.
func (mg *resultMerger) results() ([]server.Result, int) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	out := make([]server.Result, 0, len(mg.order))
	for _, h := range mg.order {
		out = append(out, *mg.byHash[h])
	}
	n := len(mg.servers)
	if mg.servers["kad"] {
		n-- // Kad is not a server
	}
	return out, n
}

// satAdd adds without wrapping around.
func satAdd(a, b uint32) uint32 {
	if a > ^uint32(0)-b {
		return ^uint32(0)
	}
	return a + b
}

// KadProblem says why Kad is not running although it is on ("" = none).
func (m *Manager) KadProblem() string { return m.cfg.KadProblem }

// KadStatus returns Kad's state, or nil when Kad is off.
func (m *Manager) KadStatus() *kad.Status {
	if m.cfg.KadStatus == nil {
		return nil
	}
	st := m.cfg.KadStatus()
	return &st
}

// ListenPortWanted is the TCP port configured for peers (0 = none),
// whether or not listening on it worked.
func (m *Manager) ListenPortWanted() uint16 { return m.cfg.Port }

// ListenPort returns the TCP port we accept peers on (0 = none).
func (m *Manager) ListenPort() uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// PublicIP returns our public IP as the connected server sees it (HighID
// only), or the zero address.
func (m *Manager) PublicIP() netip.Addr {
	m.mu.Lock()
	srv := m.srv
	m.mu.Unlock()
	if srv == nil {
		return netip.Addr{}
	}
	st := srv.Status() // once: HighID and the ID must go together
	if !st.HighID {
		return netip.Addr{}
	}
	a, _ := netip.AddrFromSlice(server.IPFromU32(st.ClientID).To4())
	return a
}

// SharingStatus returns the upload and shared-library state, if sharing
// is enabled.
func (m *Manager) SharingStatus() (*upload.Status, *share.Status) {
	var us *upload.Status
	var ss *share.Status
	if m.cfg.Upload != nil {
		st := m.cfg.Upload.Status()
		us = &st
	}
	if m.cfg.Library != nil {
		st := m.cfg.Library.Status()
		ss = &st
	}
	return us, ss
}
