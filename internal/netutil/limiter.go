package netutil

import (
	"sync"
	"sync/atomic"
	"time"
)

// Limiter spreads bytes over time to a rate (a token bucket).
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second; 0 = unlimited
	tokens float64
	last   time.Time

	stopped atomic.Bool
}

// Stop makes Wait return at once from now on (when stopping: data still
// read then is not worth waiting for).
func (l *Limiter) Stop() {
	if l != nil {
		l.stopped.Store(true)
	}
}

// NewLimiter returns a limiter of bytesPerSec (0 = unlimited).
func NewLimiter(bytesPerSec int) *Limiter {
	return &Limiter{rate: float64(bytesPerSec), tokens: float64(bytesPerSec), last: time.Now()}
}

// SetRate changes the rate (0 = unlimited) from now on.
func (l *Limiter) SetRate(bytesPerSec int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = float64(bytesPerSec)
	l.tokens = min(l.tokens, l.rate)
	l.last = time.Now()
}

// Rate returns the rate in bytes per second (0 = unlimited).
func (l *Limiter) Rate() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int(l.rate)
}

// Wait blocks until n more bytes fit the rate, and returns how long it
// waited.
func (l *Limiter) Wait(n int) time.Duration {
	if l == nil {
		return 0
	}
	var waited time.Duration
	for !l.stopped.Load() {
		l.mu.Lock()
		if l.rate <= 0 { // unlimited (perhaps since the rate changed)
			l.mu.Unlock()
			return waited
		}
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
		// In short steps, so that Stop is seen.
		wait = min(wait, 100*time.Millisecond)
		time.Sleep(wait)
		waited += wait
	}
	return waited
}
