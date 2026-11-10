package netutil

import (
	"testing"
	"time"
)

// Stop ends the waits at once.
func TestLimiterStop(t *testing.T) {
	l := NewLimiter(1000)
	l.Wait(1000) // the bucket empty
	done := make(chan struct{})
	go func() { l.Wait(1000); close(done) }()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	l.Stop()
	<-done
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("Wait went on after Stop")
	}
	if w := l.Wait(1 << 20); w != 0 {
		t.Fatalf("waited %v after Stop", w)
	}
}

// The rate can change while bytes wait: lifting the limit frees them.
func TestLimiterSetRate(t *testing.T) {
	l := NewLimiter(1000)
	l.Wait(1000) // the bucket is empty now
	done := make(chan time.Duration)
	go func() { done <- l.Wait(100_000) }() // 100 s at 1 KB/s
	time.Sleep(50 * time.Millisecond)
	l.SetRate(0)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("still waiting after the limit was lifted")
	}
	if l.Rate() != 0 {
		t.Fatal("rate")
	}
	l.SetRate(50_000)
	start := time.Now()
	l.Wait(25_000)
	l.Wait(25_000)
	if time.Since(start) > 2*time.Second {
		t.Fatal("new rate not used")
	}
}
