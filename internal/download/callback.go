package download

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"mymule/internal/netutil"
)

// Limits on server callback requests. A legitimate LowID client gets a few
// of these; a malicious server could send thousands to turn its users into
// a flood against someone else, or to make them connect into their own LAN.
const (
	maxCallbacksActive  = 5                // connections in progress at once
	maxCallbacksPerMin  = 30               // started per minute
	callbackRepeatAfter = 60 * time.Second // same address at most this often
)

// callbackGate decides which callback requests we act on.
type callbackGate struct {
	mu      sync.Mutex
	active  int
	started []time.Time          // start times within the last minute
	last    map[string]time.Time // address -> last time we called it
}

// admit reports whether to connect to ip:port, and if so counts the
// connection as active until done is called. highID is our own state: a
// server only asks LowID clients to call back, because a HighID client can
// simply be connected to.
func (g *callbackGate) admit(ip net.IP, port uint16, highID bool, now time.Time) (ok bool, why string) {
	if highID {
		return false, "we are HighID"
	}
	if port == 0 || !netutil.IsPublicIPv4(ip) {
		return false, "address is not a public one"
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= maxCallbacksActive {
		return false, "too many callbacks in progress"
	}
	recent := g.started[:0]
	for _, t := range g.started {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	g.started = recent
	if len(g.started) >= maxCallbacksPerMin {
		return false, "too many callbacks this minute"
	}
	if t, seen := g.last[addr]; seen && now.Sub(t) < callbackRepeatAfter {
		return false, "address called back recently"
	}
	if g.last == nil {
		g.last = map[string]time.Time{}
	}
	for a, t := range g.last {
		if now.Sub(t) >= callbackRepeatAfter {
			delete(g.last, a)
		}
	}
	g.last[addr] = now
	g.started = append(g.started, now)
	g.active++
	return true, ""
}

// done ends a connection counted by admit.
func (g *callbackGate) done() {
	g.mu.Lock()
	if g.active > 0 {
		g.active--
	}
	g.mu.Unlock()
}

// onCallback handles OP_CALLBACKREQUESTED: while we are LowID, another
// client that wants to talk to us asks, through the server, that we
// connect to it.
func (m *Manager) onCallback(ctx context.Context, ip net.IP, port uint16) {
	m.mu.Lock()
	highID := m.srv != nil && m.srv.Status().HighID
	m.mu.Unlock()
	if ok, why := m.callbacks.admit(ip, port, highID, time.Now()); !ok {
		m.cfg.Debug("server callback to %s:%d ignored: %s", ip, port, why)
		return
	}
	defer m.callbacks.done()
	m.dialCallback(ctx, net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
}

// dialCallback connects to addr and runs a session on it.
func (m *Manager) dialCallback(ctx context.Context, addr string) {
	d := net.Dialer{Timeout: ConnectTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return
	}
	m.wg.Add(1)
	defer m.wg.Done()
	m.runSession(ctx, nc, true, nil)
}
