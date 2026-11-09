package netutil

import (
	"sync"
	"time"
)

// Limiter spreads bytes over time to a rate (a token bucket).
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second; 0 = unlimited
	tokens float64
	last   time.Time
}

// NewLimiter returns a limiter of bytesPerSec (0 = unlimited).
func NewLimiter(bytesPerSec int) *Limiter {
	return &Limiter{rate: float64(bytesPerSec), tokens: float64(bytesPerSec), last: time.Now()}
}

// Wait blocks until n more bytes fit the rate, and returns how long it
// waited.
func (l *Limiter) Wait(n int) time.Duration {
	if l == nil || l.rate <= 0 {
		return 0
	}
	var waited time.Duration
	for {
		l.mu.Lock()
		now := time.Now()
		// The bucket holds at least one packet: with a limit below the
		// packet size (1-9 KB/s) it could otherwise never fill enough,
		// and everything waiting hung for good.
		l.tokens = min(max(l.rate, float64(n)), l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= float64(n) {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return waited
		}
		wait := time.Duration((float64(n) - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		time.Sleep(wait)
		waited += wait
	}
}
