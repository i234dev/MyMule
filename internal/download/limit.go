package download

import (
	"net"
	"sync"
)

// Limits on incoming peer connections. Our TCP port is open to the whole
// Internet, so without them anyone could open thousands of idle
// connections and exhaust our file descriptors.
const (
	MaxIncoming      = 300 // in total
	MaxIncomingPerIP = 5   // from one address
)

// connLimiter counts incoming connections, in total and per IP.
// The zero value uses MaxIncoming and MaxIncomingPerIP.
type connLimiter struct {
	mu       sync.Mutex
	total    int
	perIP    map[string]int
	maxTotal int
	maxPerIP int
}

// acquire reserves a slot for a connection from ip. It reports false,
// and reserves nothing, when a limit is reached.
func (l *connLimiter) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	maxTotal, maxPerIP := l.maxTotal, l.maxPerIP
	if maxTotal <= 0 {
		maxTotal = MaxIncoming
	}
	if maxPerIP <= 0 {
		maxPerIP = MaxIncomingPerIP
	}
	if l.total >= maxTotal || l.perIP[ip] >= maxPerIP {
		return false
	}
	if l.perIP == nil {
		l.perIP = map[string]int{}
	}
	l.total++
	l.perIP[ip]++
	return true
}

// release frees a slot taken by acquire.
func (l *connLimiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perIP[ip] <= 0 {
		return
	}
	l.total--
	if l.perIP[ip]--; l.perIP[ip] == 0 {
		delete(l.perIP, ip)
	}
}

// remoteIP returns the address part of a connection's remote end.
func remoteIP(nc net.Conn) string {
	host, _, err := net.SplitHostPort(nc.RemoteAddr().String())
	if err != nil {
		return nc.RemoteAddr().String()
	}
	return host
}
