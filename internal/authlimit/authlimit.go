// Package authlimit slows down guessing of the api_key. After too many
// wrong keys from one address within a window, every key attempt from that
// address (right or wrong) is refused for a while, so the key cannot be
// found by trying, whichever way it is sent (API header or ?apikey=, web
// login, qBittorrent login, Torznab).
package authlimit

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Defaults: 10 wrong keys within 10 minutes block the address for 15.
const (
	MaxFails = 10
	Window   = 10 * time.Minute
	Block    = 15 * time.Minute
)

// Limiter counts failed attempts per client address.
type Limiter struct {
	maxFails      int
	window, block time.Duration
	now           func() time.Time

	mu    sync.Mutex
	state map[string]*entry
}

type entry struct {
	fails        []time.Time
	blockedUntil time.Time
}

// New returns a limiter with the given policy.
func New(maxFails int, window, block time.Duration) *Limiter {
	return &Limiter{maxFails: maxFails, window: window, block: block, now: time.Now, state: map[string]*entry{}}
}

// Default is shared by every place that checks the api_key.
var Default = New(MaxFails, Window, Block)

// clientKey groups addresses: one IPv4 address, or an IPv6 /64 (one
// customer usually owns a whole /64 and could rotate through it).
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Allowed reports whether r's address may try a key now.
func (l *Limiter) Allowed(r *http.Request) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.state[clientKey(r)]
	return e == nil || !l.now().Before(e.blockedUntil)
}

// Failed records a wrong key from r's address.
func (l *Limiter) Failed(r *http.Request) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.state) > 10000 {
		l.pruneLocked(now)
	}
	k := clientKey(r)
	e := l.state[k]
	if e == nil {
		e = &entry{}
		l.state[k] = e
	}
	recent := e.fails[:0]
	for _, t := range e.fails {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	e.fails = append(recent, now)
	if len(e.fails) >= l.maxFails {
		e.blockedUntil = now.Add(l.block)
		e.fails = nil
	}
}

func (l *Limiter) pruneLocked(now time.Time) {
	for k, e := range l.state {
		if now.After(e.blockedUntil) && (len(e.fails) == 0 || now.Sub(e.fails[len(e.fails)-1]) >= l.window) {
			delete(l.state, k)
		}
	}
}
