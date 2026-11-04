package download

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"mymule/internal/ed2k"
	"mymule/internal/server"
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
		pf, err := Create(m.cfg.TempDir, link)
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
	if finished {
		return f.pf.Name, nil
	}
	return f.pf.Name, f.pf.Delete()
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
